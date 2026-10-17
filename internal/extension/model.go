package extension

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"

	"agentbox/internal/config"
	"agentbox/pkg/extspi"
	"agentbox/pkg/modelgw"
)

// The model.request / model.stream hooks. They fire for requests passing
// through the model gateway (internal/server/modelgateway.go), which hands
// each one to Inspector; everything here is specific to those hooks, the
// instance lifecycle in instance.go is not.

var detailKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// modelHooks are the hooks served by this file.
var modelHooks = []string{extspi.HookModelRequest, extspi.HookModelStream}

// implementsHook reports whether handler implements the interface a hook
// needs. Hook groups without an interface yet are rejected.
func implementsHook(handler extspi.Handler, spec extspi.HookSpec) bool {
	switch spec.Group {
	case "model":
		_, ok := handler.(extspi.ModelHandler)
		return ok
	}
	return false
}

// modelState is what an instance keeps for the model hooks: the salt for
// request fingerprints and the streams it currently watches. Guarded by the
// instance's mutex.
type modelState struct {
	salt      [32]byte
	streams   map[string]*hostStream
	closedIDs []string // recently closed, so a late abort is "closed" rather than unknown
}

// dropStreamsLocked forgets every stream the instance watches and returns
// them.
func (in *instance) dropStreamsLocked(remember bool) []*hostStream {
	out := make([]*hostStream, 0, len(in.model.streams))
	for id, s := range in.model.streams {
		out = append(out, s)
		s.detach(in)
		if remember {
			in.rememberClosedLocked(id)
		}
	}
	in.model.streams = map[string]*hostStream{}
	return out
}

func (in *instance) rememberClosedLocked(id string) {
	in.model.closedIDs = append(in.model.closedIDs, id)
	if len(in.model.closedIDs) > recentClosed {
		in.model.closedIDs = in.model.closedIDs[len(in.model.closedIDs)-recentClosed:]
	}
}

// failStreams applies the failure policy to streams a crashed instance was
// watching: fail_closed interrupts them.
func (in *instance) failStreams(streams []*hostStream) {
	if in.policy != FailClosed {
		return
	}
	for _, s := range streams {
		s.hostAbort(in, "extension_unavailable", "The extension that inspects this request stopped responding; this reply was interrupted.")
	}
}

// modelRequest runs model/request with the host deadline. A panic is a crash.
func (in *instance) modelRequest(ctx context.Context, p extspi.ModelRequestParams) (extspi.RequestVerdict, error) {
	handler, gen, _, ok := in.running()
	mh, isModel := handler.(extspi.ModelHandler)
	if !ok || !isModel {
		return extspi.RequestVerdict{}, errUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, in.h.opts.RequestTimeout)
	defer cancel()
	type result struct {
		v   extspi.RequestVerdict
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: errors.New("model/request panicked")}
				in.crash(gen, errors.New("model/request handler panicked"))
			}
		}()
		v, err := mh.ModelRequest(ctx, p)
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return extspi.RequestVerdict{}, ctx.Err()
	}
}

// fingerprint salts the gateway fingerprint per instance so extensions cannot
// correlate requests with each other or test guesses against known prompts.
func (in *instance) fingerprint(fp [32]byte) string {
	mac := hmac.New(sha256.New, in.model.salt[:])
	mac.Write(fp[:])
	return hex.EncodeToString(mac.Sum(nil))
}

func (ih *instanceHost) Abort(_ context.Context, p extspi.AbortParams) (extspi.AbortResult, error) {
	in := ih.in
	if !ih.current() {
		return extspi.AbortResult{}, &extspi.Error{Code: extspi.CodeInvalidRequest, Message: "handler is no longer active"}
	}
	if _, _, hooks, _ := in.running(); !hooks[extspi.HookModelStream] {
		return extspi.AbortResult{}, &extspi.Error{Code: extspi.CodePermissionDenied, Message: "not subscribed to model.stream"}
	}
	if _, ok := in.pkg.Manifest.ErrorDef(p.Code); !ok {
		return extspi.AbortResult{}, &extspi.Error{Code: extspi.CodeInvalidVerdict, Message: "error code is not declared in the manifest"}
	}
	if err := plainText(p.Message, 500, false); err != nil {
		return extspi.AbortResult{}, &extspi.Error{Code: extspi.CodeInvalidVerdict, Message: "message " + err.Error()}
	}
	if len(p.Detail) > 16 {
		return extspi.AbortResult{}, &extspi.Error{Code: extspi.CodeInvalidVerdict, Message: "detail has more than 16 keys"}
	}
	for k, v := range p.Detail {
		if !detailKeyRe.MatchString(k) || math.IsNaN(v) || math.IsInf(v, 0) {
			return extspi.AbortResult{}, &extspi.Error{Code: extspi.CodeInvalidVerdict, Message: "detail must map short names to finite numbers"}
		}
	}
	in.mu.Lock()
	s := in.model.streams[p.Stream]
	known := s != nil || slices.Contains(in.model.closedIDs, p.Stream)
	in.mu.Unlock()
	if s == nil {
		if known {
			return extspi.AbortResult{Applied: false, Reason: "closed"}, nil
		}
		return extspi.AbortResult{}, &extspi.Error{Code: extspi.CodeUnknownStream, Message: "unknown stream"}
	}
	outcome := s.abortBy(in, p.Code, p.Message)
	if outcome == "applied" {
		in.h.record(in, Event{Kind: "abort", Code: p.Code, Stream: p.Stream, Session: s.sc.Session, Account: s.sc.Account, User: s.sc.User, Detail: p.Detail})
		return extspi.AbortResult{Applied: true}, nil
	}
	return extspi.AbortResult{Applied: false, Reason: outcome}, nil
}

// StreamContext identifies whose model traffic a gateway request carries.
type StreamContext struct {
	Session string
	Account string
	User    string
	Agent   string
}

// spiContext is sent only with the model.context permission. The gateway
// cannot tell a web chat turn from a terminal CLI in the same container, so
// the source is always unknown rather than guessed.
func (sc StreamContext) spiContext() *extspi.Context {
	return &extspi.Context{Session: sc.Session, Account: sc.Account, User: sc.User, Source: "unknown"}
}

// Inspector returns the inspector for one request's account, or nil when no
// enabled extension with a model hook is bound to it.
func (h *Host) Inspector(sc StreamContext) modelgw.Inspector {
	h.mu.Lock()
	defer h.mu.Unlock()
	var list []*instance
	for _, inst := range h.orderedLocked() {
		if inst.uses(modelHooks) && inst.setting.Binds(config.BindingTarget{Account: sc.Account, User: sc.User}) {
			list = append(list, inst)
		}
	}
	if len(list) == 0 {
		return nil
	}
	return &inspector{h: h, sc: sc, instances: list}
}

type inspector struct {
	h         *Host
	sc        StreamContext
	instances []*instance
}

func newStreamID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "s_" + hex.EncodeToString(b[:])
}

func decorate(message string, in *instance) string {
	return message + " [" + in.id() + "]"
}

const unavailableMessage = "The extension that inspects this request is unavailable; the request was not sent."

func (x *inspector) Begin(ctx context.Context, req modelgw.Request) (modelgw.Stream, *modelgw.Verdict) {
	s := &hostStream{h: x.h, id: newStreamID(), sc: x.sc, protocol: string(req.Protocol), abort: req.Abort}
	var requested []*instance
	reject := func(code, message string) (modelgw.Stream, *modelgw.Verdict) {
		for _, in := range requested {
			in.enqueue(&streamClose{p: extspi.StreamCloseParams{Stream: s.id, Reason: extspi.CloseRejected}})
		}
		return nil, &modelgw.Verdict{Code: code, Message: message}
	}
	for _, in := range x.instances {
		wantsRequest := in.pkg.Manifest.HasHook(extspi.HookModelRequest) && in.granted[extspi.PermRequestFingerprint]
		wantsStream := in.pkg.Manifest.HasHook(extspi.HookModelStream) && in.granted[extspi.PermStreamText]
		if !wantsRequest && !wantsStream {
			continue
		}
		_, _, hooks, ok := in.running()
		if !ok {
			if in.policy == FailClosed {
				x.h.record(in, Event{Kind: "unavailable", Code: "extension_unavailable", Session: x.sc.Session, Account: x.sc.Account, User: x.sc.User})
				return reject("extension_unavailable", decorate(unavailableMessage, in))
			}
			continue
		}
		sentRequest := false
		if hooks[extspi.HookModelRequest] {
			params := extspi.ModelRequestParams{Stream: s.id, Protocol: string(req.Protocol), Method: req.Method, Path: req.Path, Agent: x.sc.Agent,
				Fingerprint: in.fingerprint(req.Fingerprint)}
			if in.granted[extspi.PermContext] {
				params.Context = x.sc.spiContext()
			}
			v, err := in.modelRequest(ctx, params)
			sentRequest = true
			requested = append(requested, in)
			if err == nil {
				switch v.Action {
				case extspi.ActionAllow:
				case extspi.ActionReject:
					if _, declared := in.pkg.Manifest.ErrorDef(v.Code); !declared || plainText(v.Message, 500, false) != nil {
						err = &extspi.Error{Code: extspi.CodeInvalidVerdict, Message: "rejection must use a declared code and a short plain-text message"}
					}
				default:
					err = &extspi.Error{Code: extspi.CodeInvalidVerdict, Message: "unknown action"}
				}
			}
			if err != nil {
				in.log("warn", "model/request failed: "+err.Error())
				if in.policy == FailClosed {
					x.h.record(in, Event{Kind: "unavailable", Code: "extension_unavailable", Stream: s.id, Session: x.sc.Session, Account: x.sc.Account, User: x.sc.User})
					return reject("extension_unavailable", decorate(unavailableMessage, in))
				}
			} else if v.Action == extspi.ActionReject {
				x.h.record(in, Event{Kind: "reject", Code: v.Code, Stream: s.id, Session: x.sc.Session, Account: x.sc.Account, User: x.sc.User})
				return reject(v.Code, decorate(v.Message, in))
			}
		}
		streaming := hooks[extspi.HookModelStream]
		if streaming || sentRequest {
			s.parts = append(s.parts, &participant{in: in, stream: streaming})
		}
	}
	if len(s.parts) == 0 {
		return nil, nil
	}
	for _, p := range s.parts {
		if !p.in.register(s) {
			p.detached.Store(true)
		}
	}
	return s, nil
}

type participant struct {
	in       *instance
	stream   bool // subscribed to model.stream; otherwise only gets close
	detached atomic.Bool
	seq      int64 // last delta batch delivered; only the instance's dispatcher touches it
}

// hostStream fans one gateway stream out to the participating instances.
// parts is fixed after Begin; detached flags are atomic so instances can drop
// a stream while holding their own lock without lock-order inversions.
type hostStream struct {
	h        *Host
	id       string
	sc       StreamContext
	protocol string
	abort    func(modelgw.Verdict) modelgw.AbortOutcome
	parts    []*participant

	mu        sync.Mutex
	abortedBy string
}

func (in *instance) register(s *hostStream) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.state != "running" && in.state != "degraded" {
		return false
	}
	in.model.streams[s.id] = s
	return true
}

func (in *instance) unregister(id string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, ok := in.model.streams[id]; ok {
		delete(in.model.streams, id)
		in.rememberClosedLocked(id)
	}
}

// detach is called by an instance (under its lock) that stops watching s.
func (s *hostStream) detach(in *instance) {
	for _, p := range s.parts {
		if p.in == in {
			p.detached.Store(true)
		}
	}
}

func (s *hostStream) Open() {
	for _, p := range s.parts {
		if !p.stream || p.detached.Load() {
			continue
		}
		open := extspi.StreamOpenParams{Stream: s.id, Protocol: s.protocol, Agent: s.sc.Agent}
		if p.in.granted[extspi.PermContext] {
			open.Context = s.sc.spiContext()
		}
		if p.in.enqueue(&streamOpen{p: open}) == notRunning {
			p.detached.Store(true)
		}
	}
}

// Observe hands deltas to the instances asynchronously; interruptions come
// back through Abort, so it never returns a verdict itself.
func (s *hostStream) Observe(deltas []modelgw.Delta, ended []string) *modelgw.Verdict {
	batch := make([]extspi.Delta, len(deltas))
	for i, d := range deltas {
		batch[i] = extspi.Delta{Part: d.Part, Channel: d.Channel, Text: d.Text}
	}
	for _, p := range s.parts {
		if !p.stream || p.detached.Load() {
			continue
		}
		switch p.in.enqueue(&streamDelta{part: p, stream: s.id, deltas: batch, ended: ended}) {
		case notRunning:
			p.detached.Store(true)
		case overBudget:
			s.overflow(p)
		}
	}
	return nil
}

func (s *hostStream) overflow(p *participant) {
	p.detached.Store(true)
	p.in.unregister(s.id)
	p.in.enqueue(&streamClose{p: extspi.StreamCloseParams{Stream: s.id, Reason: extspi.CloseOverflow}})
	p.in.log("warn", "stream inspection fell behind and was dropped")
	if p.in.policy == FailClosed {
		s.hostAbort(p.in, "extension_overloaded", "The extension that inspects this reply fell behind; this reply was interrupted.")
		return
	}
	s.h.record(p.in, Event{Kind: "overloaded", Code: "extension_overloaded", Stream: s.id, Session: s.sc.Session, Account: s.sc.Account, User: s.sc.User})
}

func (s *hostStream) Close(c modelgw.Close) {
	s.mu.Lock()
	abortedBy := s.abortedBy
	s.mu.Unlock()
	params := extspi.StreamCloseParams{Stream: s.id, Reason: string(c.Reason)}
	if c.Verdict != nil && c.Reason == modelgw.Aborted {
		params.AbortedBy, params.Code = abortedBy, c.Verdict.Code
	}
	for _, p := range s.parts {
		if p.detached.Load() {
			continue
		}
		p.in.unregister(s.id)
		p.in.enqueue(&streamClose{p: params})
	}
}

// abortBy applies an extension's interruption and reports the outcome.
func (s *hostStream) abortBy(in *instance, code, message string) string {
	outcome := s.abort(modelgw.Verdict{Code: code, Message: decorate(message, in)})
	if outcome == modelgw.AbortApplied {
		s.mu.Lock()
		s.abortedBy = in.id()
		s.mu.Unlock()
	}
	return string(outcome)
}

// hostAbort interrupts a stream on the host's own authority (fail_closed).
func (s *hostStream) hostAbort(in *instance, code, message string) {
	if s.abortBy(in, code, message) == string(modelgw.AbortApplied) {
		s.h.record(in, Event{Kind: "unavailable", Code: code, Stream: s.id, Session: s.sc.Session, Account: s.sc.Account, User: s.sc.User})
	}
}

// Stream notices. Consecutive delta batches of one stream coalesce until a
// batch ends a part, so a slow extension catches up instead of lagging.

type streamOpen struct{ p extspi.StreamOpenParams }

func (n *streamOpen) size() int                { return 0 }
func (n *streamOpen) absorb(notice) bool       { return false }
func (n *streamOpen) deliver(h extspi.Handler) { h.(extspi.ModelHandler).StreamOpen(n.p) }

type streamClose struct{ p extspi.StreamCloseParams }

func (n *streamClose) size() int                { return 0 }
func (n *streamClose) absorb(notice) bool       { return false }
func (n *streamClose) deliver(h extspi.Handler) { h.(extspi.ModelHandler).StreamClose(n.p) }

type streamDelta struct {
	part   *participant
	stream string
	deltas []extspi.Delta
	ended  []string
	owned  bool // deltas is this notice's own copy, safe to append to
	bytes  int
}

func (n *streamDelta) size() int {
	if n.bytes == 0 {
		for _, d := range n.deltas {
			n.bytes += len(d.Text)
		}
	}
	return n.bytes
}

func (n *streamDelta) absorb(next notice) bool {
	d, ok := next.(*streamDelta)
	if !ok || d.stream != n.stream || len(n.ended) != 0 {
		return false
	}
	if !n.owned {
		// The batch slice is shared with the other instances watching the
		// stream; never append into it.
		n.deltas = append(make([]extspi.Delta, 0, len(n.deltas)+len(d.deltas)), n.deltas...)
		n.owned = true
	}
	n.size()
	n.deltas = append(n.deltas, d.deltas...)
	n.ended = d.ended
	n.bytes += d.size()
	return true
}

func (n *streamDelta) deliver(h extspi.Handler) {
	n.part.seq++
	h.(extspi.ModelHandler).StreamDelta(extspi.StreamDeltaParams{Stream: n.stream, Seq: n.part.seq, Deltas: n.deltas, EndedParts: n.ended})
}
