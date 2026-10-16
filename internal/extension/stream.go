package extension

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"

	"agentbox/pkg/extspi"
	"agentbox/pkg/modelgw"
)

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
// enabled extension is bound to it.
func (h *Host) Inspector(sc StreamContext) modelgw.Inspector {
	h.mu.Lock()
	defer h.mu.Unlock()
	var list []*instance
	for _, inst := range h.orderedLocked() {
		if inst.setting.Binds(sc.Account) {
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
			in.enqueue(work{kind: workClose, stream: s.id, close: extspi.StreamCloseParams{Stream: s.id, Reason: extspi.CloseRejected}})
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
			v, err := in.request(ctx, params)
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
	in.streams[s.id] = s
	return true
}

func (in *instance) unregister(id string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, ok := in.streams[id]; ok {
		delete(in.streams, id)
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
		if p.in.enqueue(work{kind: workOpen, stream: s.id, open: open}) == notRunning {
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
		switch p.in.enqueue(work{kind: workDelta, stream: s.id, deltas: batch, ended: ended}) {
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
	p.in.enqueue(work{kind: workClose, stream: s.id, close: extspi.StreamCloseParams{Stream: s.id, Reason: extspi.CloseOverflow}})
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
		p.in.enqueue(work{kind: workClose, stream: s.id, close: params})
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
