package extension

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"agentbox/internal/config"
	"agentbox/pkg/extspi"
)

const (
	queueBudget     = 4 << 20 // bytes of notification payload waiting for one instance
	initTimeout     = 5 * time.Second
	shutdownTimeout = 2 * time.Second
	pingInterval    = 30 * time.Second
	maxCrashes      = 5
	crashWindow     = 10 * time.Minute
	maxBackoff      = 60 * time.Second
	recentClosed    = 256
)

var errUnavailable = errors.New("extension unavailable")

// notice is one queued notification for an instance, delivered in order by
// the instance's dispatcher. Hook surfaces define their own notices (see
// model.go).
type notice interface {
	// size is counted against the queue budget; 0 always fits.
	size() int
	// absorb merges the next queued notice into this one, so a slow
	// extension catches up instead of lagging. It reports false when the two
	// cannot merge.
	absorb(next notice) bool
	deliver(h extspi.Handler)
}

type enqueueResult int

const (
	queued enqueueResult = iota
	notRunning
	overBudget
)

// instance is one running extension. Notifications go through an ordered
// queue served by a single dispatcher goroutine; synchronous hook calls (such
// as model/request) run on the caller's goroutine with a deadline.
type instance struct {
	h       *Host
	pkg     *Package
	setting config.ExtensionSetting
	policy  string
	granted map[string]bool

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}

	mu          sync.Mutex
	state       string
	lastError   string
	startedAt   time.Time
	restarts    int
	crashes     []time.Time
	gen         int
	handler     extspi.Handler
	hooks       map[string]bool
	reported    string
	metrics     map[string]float64
	queue       []notice
	queuedBytes int
	model       modelState // model.* hook state, see model.go
	logs        ring[LogLine]
	events      ring[Event]
	logSecond   int64
	logCount    int
	droppedLogs int64
}

func newInstance(h *Host, p *Package, s config.ExtensionSetting, policy string) *instance {
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{h: h, pkg: p, setting: s.Clone(), policy: policy, granted: map[string]bool{},
		ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1),
		state: "starting", metrics: map[string]float64{}, model: modelState{streams: map[string]*hostStream{}}}
	for _, perm := range s.Permissions {
		inst.granted[perm] = true
	}
	_, _ = rand.Read(inst.model.salt[:])
	return inst
}

func (in *instance) id() string { return in.pkg.Manifest.ID }

func (in *instance) stop() { in.cancel() }

func (in *instance) signal() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// run starts the handler, serves its queue, and restarts it with backoff
// after a crash until the instance is stopped or fails too often.
func (in *instance) run() {
	defer close(in.done)
	defer func() {
		in.mu.Lock()
		in.state = "stopped"
		in.dropStreamsLocked(false)
		in.queue, in.queuedBytes = nil, 0
		in.mu.Unlock()
	}()
	backoff := time.Second
	for {
		if err := in.start(); err != nil {
			in.crash(in.currentGen(), err)
		} else if in.serve() {
			return
		}
		in.mu.Lock()
		failed := in.state == "failed"
		in.mu.Unlock()
		if failed {
			<-in.ctx.Done()
			return
		}
		select {
		case <-in.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
		in.mu.Lock()
		in.restarts++
		in.mu.Unlock()
	}
}

func (in *instance) currentGen() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.gen
}

func (in *instance) start() (err error) {
	handler := in.pkg.newHandler()
	if in.h.opts.PipeBuiltins {
		handler = extspi.Pipe(handler)
	}
	in.mu.Lock()
	in.gen++
	gen := in.gen
	in.handler, in.hooks, in.reported = handler, nil, ""
	in.state = "starting"
	in.mu.Unlock()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("initialize panicked")
		}
	}()
	ctx, cancel := context.WithTimeout(in.ctx, initTimeout)
	defer cancel()
	permissions := make([]string, 0, len(in.granted))
	for perm := range in.granted {
		permissions = append(permissions, perm)
	}
	slices.Sort(permissions)
	res, err := handler.Initialize(ctx, extspi.InitializeParams{
		SPIVersions: []string{extspi.Version},
		Host:        extspi.HostInfo{Name: "agentbox", Version: in.h.opts.HostVersion},
		Extension:   extspi.ExtensionInfo{ID: in.id(), Version: in.pkg.Manifest.Version},
		Config:      in.setting.Config,
		Permissions: permissions,
		Limits:      extspi.Limits{RequestTimeoutMS: int(in.h.opts.RequestTimeout / time.Millisecond), MaxMessageBytes: extspi.MaxMessageBytes},
	}, &instanceHost{in: in, gen: gen})
	if err != nil {
		return fmt.Errorf("initialize failed: %w", err)
	}
	if major, _, ok := splitVersion(res.SPIVersion); !ok || major != 1 {
		return fmt.Errorf("initialize returned unsupported spi_version %q", res.SPIVersion)
	}
	hooks := map[string]bool{}
	for _, hook := range res.Hooks {
		spec, ok := extspi.LookupHook(hook)
		if !ok || !in.pkg.Manifest.HasHook(hook) || !in.granted[spec.Permission] {
			return fmt.Errorf("initialize subscribed to undeclared or ungranted hook %q", hook)
		}
		if !implementsHook(handler, spec) {
			return fmt.Errorf("initialize subscribed to %q without implementing its interface", hook)
		}
		hooks[hook] = true
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if gen != in.gen {
		return errors.New("superseded")
	}
	in.hooks = hooks
	in.state, in.lastError = "running", ""
	in.startedAt = time.Now().UTC()
	return nil
}

// serve dispatches queued notifications. It returns true when the instance
// was stopped and false after a crash.
func (in *instance) serve() bool {
	in.mu.Lock()
	gen, handler := in.gen, in.handler
	in.mu.Unlock()
	var handlerDone <-chan struct{}
	if d, ok := handler.(interface{ Done() <-chan struct{} }); ok {
		handlerDone = d.Done()
	}
	pinger, _ := handler.(interface{ Ping(context.Context) error })
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	misses := 0
	for {
		item, ok := in.pop()
		if ok {
			if err := in.deliver(handler, item); err != nil {
				in.crash(gen, err)
				return false
			}
			continue
		}
		select {
		case <-in.ctx.Done():
			in.shutdown(handler)
			return true
		case <-handlerDone:
			in.crash(gen, errors.New("extension connection ended"))
			return false
		case <-ticker.C:
			if pinger == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(in.ctx, 5*time.Second)
			err := pinger.Ping(ctx)
			cancel()
			if err == nil {
				misses = 0
			} else if misses++; misses >= 3 {
				in.crash(gen, errors.New("extension stopped answering ping"))
				return false
			}
		case <-in.wake:
		}
	}
}

func (in *instance) shutdown(handler extspi.Handler) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	func() {
		defer func() { _ = recover() }()
		_ = handler.Shutdown(ctx)
	}()
}

// pop takes the next notification, merging the ones queued behind it that
// it can absorb.
func (in *instance) pop() (notice, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.queue) == 0 {
		return nil, false
	}
	item, n := in.queue[0], 1
	bytes := item.size()
	for n < len(in.queue) && item.absorb(in.queue[n]) {
		bytes += in.queue[n].size()
		n++
	}
	clear(in.queue[:n])
	in.queue = in.queue[n:]
	in.queuedBytes -= bytes
	return item, true
}

func (in *instance) deliver(handler extspi.Handler, item notice) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("extension handler panicked")
		}
	}()
	item.deliver(handler)
	return nil
}

// enqueue adds a notification unless the instance is not running or the
// queue budget is exhausted (notices of size 0 always fit).
func (in *instance) enqueue(item notice) enqueueResult {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.state != "running" && in.state != "degraded" {
		return notRunning
	}
	n := item.size()
	if n > 0 && in.queuedBytes+n > queueBudget {
		return overBudget
	}
	in.queue = append(in.queue, item)
	in.queuedBytes += n
	in.signal()
	return queued
}

// crash handles a failed handler generation: streams it was watching follow
// the failure policy, and the instance restarts unless it failed too often.
func (in *instance) crash(gen int, err error) {
	in.mu.Lock()
	if gen != in.gen || in.ctx.Err() != nil {
		in.mu.Unlock()
		return
	}
	in.gen++ // stale callbacks from the crashed handler are ignored from now on
	if c, ok := in.handler.(interface{ Close() }); ok {
		c.Close()
	}
	now := time.Now()
	in.crashes = append(slices.DeleteFunc(in.crashes, func(t time.Time) bool { return now.Sub(t) > crashWindow }), now)
	in.lastError = sanitizeLine(err.Error(), 300)
	in.state = "restarting"
	if len(in.crashes) >= maxCrashes {
		in.state = "failed"
	}
	in.queue, in.queuedBytes = nil, 0
	streams := in.dropStreamsLocked(true)
	in.mu.Unlock()
	in.h.opts.Logf("extension %s: %s (%s)", in.id(), in.lastError, in.state)
	in.failStreams(streams)
}

// running returns the handler if it can take a request now.
func (in *instance) running() (extspi.Handler, int, map[string]bool, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.state != "running" && in.state != "degraded" {
		return nil, 0, nil, false
	}
	return in.handler, in.gen, in.hooks, true
}

func (in *instance) status() StatusView {
	in.mu.Lock()
	defer in.mu.Unlock()
	v := StatusView{State: in.state, Error: in.lastError, Restarts: in.restarts, ActiveStreams: len(in.model.streams), DroppedLogs: in.droppedLogs}
	if v.State == "running" && in.reported == "degraded" {
		v.State = "degraded"
	}
	if !in.startedAt.IsZero() {
		t := in.startedAt
		v.StartedAt = &t
	}
	for hook := range in.hooks {
		v.Hooks = append(v.Hooks, hook)
	}
	slices.Sort(v.Hooks)
	if len(in.metrics) != 0 {
		v.Metrics = make(map[string]float64, len(in.metrics))
		for k, m := range in.metrics {
			v.Metrics[k] = m
		}
	}
	return v
}

func (in *instance) log(level, message string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	sec := time.Now().Unix()
	if sec != in.logSecond {
		in.logSecond, in.logCount = sec, 0
	}
	if in.logCount >= 20 {
		in.droppedLogs++
		return
	}
	in.logCount++
	in.logs.next++
	in.logs.add(LogLine{Seq: in.logs.next, Time: time.Now().UTC(), Level: level, Message: sanitizeLine(message, 1024)})
}

// instanceHost is the host one handler generation sees. It implements
// extspi.ModelHost; Abort (model.go) refuses extensions not watching streams.
type instanceHost struct {
	in  *instance
	gen int
}

func (ih *instanceHost) current() bool { return ih.in.currentGen() == ih.gen }

func (ih *instanceHost) ReportStatus(p extspi.StatusReport) {
	if !ih.current() {
		return
	}
	in := ih.in
	in.mu.Lock()
	defer in.mu.Unlock()
	if p.State == "ok" || p.State == "degraded" {
		in.reported = p.State
	}
	for name, v := range p.Metrics {
		if in.pkg.Manifest.HasMetric(name) && !math.IsNaN(v) && !math.IsInf(v, 0) {
			in.metrics[name] = v
		}
	}
}

func (ih *instanceHost) Log(p extspi.LogParams) {
	if !ih.current() {
		return
	}
	level := p.Level
	if level != "warn" && level != "error" {
		level = "info"
	}
	if !utf8.ValidString(p.Message) {
		p.Message = "(invalid UTF-8)"
	}
	ih.in.log(level, p.Message)
}
