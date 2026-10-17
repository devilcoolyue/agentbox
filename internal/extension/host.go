package extension

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sort"
	"sync"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/extension/builtin"
	"agentbox/pkg/extspi"
)

// Options configures a Host.
type Options struct {
	HostVersion string
	Builtins    []builtin.Entry
	// PipeBuiltins runs builtin handlers behind the NDJSON transport, proving
	// they do not depend on in-process calls. Used by conformance tests.
	PipeBuiltins   bool
	RequestTimeout time.Duration // model/request deadline, default 200ms
	// OnEvent observes interruptions and rejections (for server logs). It
	// must return promptly.
	OnEvent func(Event)
	Logf    func(format string, args ...any)
}

// Package is an installed extension. P1 only has builtin packages.
type Package struct {
	Manifest   Manifest
	Builtin    bool
	newHandler func() extspi.Handler
}

// Host runs extension instances and dispatches hooks to them. The parts of
// agentbox a hook observes call in through that hook's surface (Inspector for
// the model hooks); the host itself is not tied to any of them.
type Host struct {
	opts     Options
	packages []*Package
	byID     map[string]*Package

	mu        sync.Mutex
	closed    bool
	settings  map[string]config.ExtensionSetting // last applied, normalized
	invalid   map[string]string                  // id → why the stored setting was not applied
	instances map[string]*instance
	eventSeq  int64
}

func NewHost(opts Options) (*Host, error) {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 200 * time.Millisecond
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	h := &Host{opts: opts, byID: map[string]*Package{}, settings: map[string]config.ExtensionSetting{},
		invalid: map[string]string{}, instances: map[string]*instance{}}
	for _, entry := range opts.Builtins {
		m, err := ParseManifest(entry.Manifest, true)
		if err != nil {
			return nil, err
		}
		if h.byID[m.ID] != nil {
			return nil, fmt.Errorf("duplicate extension %s", m.ID)
		}
		p := &Package{Manifest: m, Builtin: true, newHandler: entry.New}
		h.packages = append(h.packages, p)
		h.byID[m.ID] = p
	}
	return h, nil
}

// Packages lists installed extensions in display order.
func (h *Host) Packages() []Package {
	out := make([]Package, len(h.packages))
	for i, p := range h.packages {
		out[i] = *p
	}
	return out
}

func (h *Host) Package(id string) (Package, bool) {
	p := h.byID[id]
	if p == nil {
		return Package{}, false
	}
	return *p, true
}

// Normalize validates an administrator's setting against the manifest: the
// config gets its defaults, permissions must be a subset of the declared ones.
// Structural rules (bindings, ids) are enforced by the config package.
func (h *Host) Normalize(id string, s config.ExtensionSetting) (config.ExtensionSetting, error) {
	p := h.byID[id]
	if p == nil {
		return s, fmt.Errorf("extension %q is not installed", id)
	}
	out := s.Clone()
	out.Version = p.Manifest.Version
	cfg, err := p.Manifest.ConfigSchema.Normalize(s.Config)
	if err != nil {
		return s, err
	}
	out.Config = cfg
	seen := map[string]bool{}
	out.Permissions = out.Permissions[:0]
	for _, perm := range s.Permissions {
		if !slices.Contains(p.Manifest.Permissions, perm) {
			return s, fmt.Errorf("permission %q is not requested by this extension", perm)
		}
		if !seen[perm] {
			seen[perm] = true
			out.Permissions = append(out.Permissions, perm)
		}
	}
	sort.Strings(out.Permissions)
	switch out.FailurePolicy {
	case "", FailOpen, FailClosed:
	default:
		return s, fmt.Errorf("failure_policy must be fail_open or fail_closed")
	}
	if out.Bindings.Mode == "" {
		out.Bindings.Mode = "none"
	}
	return out, nil
}

// Policy is the effective failure policy for a setting.
func (h *Host) Policy(id string, s config.ExtensionSetting) string {
	if s.FailurePolicy != "" {
		return s.FailurePolicy
	}
	if p := h.byID[id]; p != nil {
		return p.Manifest.FailurePolicy
	}
	return FailOpen
}

// Apply reconciles running instances with the stored settings. A changed
// revision restarts the instance with the new configuration.
func (h *Host) Apply(items map[string]config.ExtensionSetting) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	settings := map[string]config.ExtensionSetting{}
	invalid := map[string]string{}
	for id, s := range items {
		if h.byID[id] == nil {
			invalid[id] = "extension is not installed"
			continue
		}
		n, err := h.Normalize(id, s)
		if err != nil {
			invalid[id] = err.Error()
			continue
		}
		n.Revision = s.Revision
		settings[id] = n
	}
	h.settings, h.invalid = settings, invalid
	for id, inst := range h.instances {
		s, ok := settings[id]
		if !ok || !s.Enabled || s.Revision != inst.setting.Revision {
			inst.stop()
			delete(h.instances, id)
		}
	}
	for _, p := range h.packages {
		s, ok := settings[p.Manifest.ID]
		if !ok || !s.Enabled || h.instances[p.Manifest.ID] != nil {
			continue
		}
		inst := newInstance(h, p, s, h.Policy(p.Manifest.ID, s))
		h.instances[p.Manifest.ID] = inst
		go inst.run()
	}
}

// Close stops every instance and waits for them (bounded by ctx).
func (h *Host) Close(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	instances := make([]*instance, 0, len(h.instances))
	for id, inst := range h.instances {
		instances = append(instances, inst)
		delete(h.instances, id)
	}
	h.mu.Unlock()
	for _, inst := range instances {
		inst.stop()
	}
	for _, inst := range instances {
		select {
		case <-inst.done:
		case <-ctx.Done():
			return
		}
	}
}

// Uses reports whether an enabled extension declares one of hooks and holds
// the permission it needs, i.e. whether the infrastructure behind those hooks
// has to run.
func (h *Host) Uses(hooks ...string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.settings {
		if s.Enabled && grantsHook(h.byID[id].Manifest, s.Permissions, hooks) {
			return true
		}
	}
	return false
}

// grantsHook reports whether the manifest declares one of hooks and the
// permission that hook needs is in granted.
func grantsHook(m Manifest, granted []string, hooks []string) bool {
	for _, hook := range hooks {
		if spec, ok := extspi.LookupHook(hook); ok && m.HasHook(hook) && slices.Contains(granted, spec.Permission) {
			return true
		}
	}
	return false
}

// uses reports whether the instance may run one of hooks.
func (in *instance) uses(hooks []string) bool {
	return grantsHook(in.pkg.Manifest, in.setting.Permissions, hooks)
}

// Bound lists enabled extensions with one of hooks whose bindings include
// the account, in dispatch order, with their effective failure policy.
func (h *Host) Bound(account string, hooks ...string) []BoundExtension {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []BoundExtension
	for _, inst := range h.orderedLocked() {
		if inst.uses(hooks) && inst.setting.Binds(account) {
			out = append(out, BoundExtension{ID: inst.pkg.Manifest.ID, Name: inst.pkg.Manifest.Name, Policy: inst.policy})
		}
	}
	return out
}

// BoundExtension is what a workspace may know about an extension inspecting it.
type BoundExtension struct {
	ID     string `json:"id"`
	Name   Text   `json:"name"`
	Policy string `json:"-"`
}

func (h *Host) orderedLocked() []*instance {
	out := make([]*instance, 0, len(h.instances))
	for _, inst := range h.instances {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].setting.Order != out[j].setting.Order {
			return out[i].setting.Order < out[j].setting.Order
		}
		return out[i].pkg.Manifest.ID < out[j].pkg.Manifest.ID
	})
	return out
}

// StatusView is an extension's runtime state for the admin page.
type StatusView struct {
	State         string             `json:"state"` // disabled | invalid | starting | running | degraded | restarting | failed
	Error         string             `json:"error,omitempty"`
	StartedAt     *time.Time         `json:"started_at,omitempty"`
	Restarts      int                `json:"restarts"`
	Hooks         []string           `json:"hooks,omitempty"`
	Metrics       map[string]float64 `json:"metrics,omitempty"`
	ActiveStreams int                `json:"active_streams"`
	DroppedLogs   int64              `json:"dropped_logs,omitempty"`
}

func (h *Host) Status(id string) StatusView {
	h.mu.Lock()
	inst := h.instances[id]
	why := h.invalid[id]
	h.mu.Unlock()
	if inst == nil {
		if why != "" {
			return StatusView{State: "invalid", Error: why}
		}
		return StatusView{State: "disabled"}
	}
	return inst.status()
}

// Logs returns log lines after seq.
func (h *Host) Logs(id string, since int64) []LogLine {
	h.mu.Lock()
	inst := h.instances[id]
	h.mu.Unlock()
	if inst == nil {
		return []LogLine{}
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	out := []LogLine{}
	for _, l := range inst.logs.items {
		if l.Seq > since {
			out = append(out, l)
		}
	}
	return out
}

// Events returns the latest interruptions and rejections, newest first.
func (h *Host) Events(id string) []Event {
	h.mu.Lock()
	inst := h.instances[id]
	h.mu.Unlock()
	if inst == nil {
		return []Event{}
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	out := make([]Event, 0, len(inst.events.items))
	for i := len(inst.events.items) - 1; i >= 0; i-- {
		out = append(out, inst.events.items[i])
	}
	return out
}

func (h *Host) record(inst *instance, ev Event) {
	h.mu.Lock()
	h.eventSeq++
	ev.Seq = h.eventSeq
	h.mu.Unlock()
	ev.Time = time.Now().UTC()
	ev.Extension = inst.pkg.Manifest.ID
	inst.mu.Lock()
	inst.events.add(ev)
	inst.mu.Unlock()
	if h.opts.OnEvent != nil {
		h.opts.OnEvent(ev)
	}
}
