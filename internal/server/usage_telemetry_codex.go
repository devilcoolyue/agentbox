package server

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/internal/usage"
)

// Codex (0.163) reports one model request in two log records and names it by
// nothing but the conversation (thread) it belongs to:
//
//   - when the request goes out: codex.websocket_request (ChatGPT login, over
//     the Responses WebSocket; duration_ms is the time to send it) or
//     codex.api_request with endpoint /responses (API key / relay over HTTP;
//     duration_ms is the time to the response headers);
//   - when it ends: codex.sse_event with event.kind response.completed and
//     the request's token counts, plus ttft_ms counted from the moment the
//     request record was written to the first output item (reasoning
//     included).
//
// Neither carries a total duration; it is the time between the two records'
// event.timestamp (millisecond precision). Requests in one conversation go
// out one after another, so each completion closes the request opened last.
// Every CLI start first sends a warm-up request (no output, no ttft_ms); an
// interrupted request never completes and is replaced by the next one. The
// pair is matched to its usage row by usage.CodexRequestKey: the conversation
// and the request's token counts, which the rows record from the same CLI.
//
// Over HTTP Codex also writes one codex.sse_event per stream event; those are
// read and dropped like everything else in the export.
//
// Some requests reach no usage row any other way, and the completion record is
// all there is: the warm-up every CLI start sends over the WebSocket (it never
// enters Codex's usage counts: not the web turn's, not the terminal rollout's,
// not the title's), and the side conversation the terminal UI opens with a
// light model, which keeps no session record (rollout). Those become
// UsageKindBackground rows (recordCodexBackground). Anything else is left to
// the paths that account for it — web settlement (whose rows land only when
// the turn ends, too late to look for here), the terminal backfill, title
// rows — so only terminal-UI conversations without a rollout qualify.

// codexClockSlackMS absorbs the millisecond rounding of event.timestamp when
// the first output item arrives with the last one.
const codexClockSlackMS = 5

// codexPendingMax bounds the requests waiting for a completion; one per
// running conversation is all a healthy export needs.
const codexPendingMax = 4096

// codexPendingKeep is how long a request may wait for its completion record.
const codexPendingKeep = time.Hour

// codexTelemetryEvent is the part of one Codex log record the timing needs.
type codexTelemetryEvent struct {
	Conversation string
	Request      bool  // a model request went out; otherwise a completion
	AtMS         int64 // event.timestamp, Unix milliseconds
	DurationMS   int64 // request: time to send it / to the response headers
	TTFTMs       int64
	HasTTFT      bool
	Input        int64 // completion: tokens, input including cached
	Cached       int64
	Output       int64
	Model        string
	Effort       string
	Originator   string // codex-tui, codex_exec, or the app-server client's name
}

// codexTerminalOriginator is the terminal UI's originator, the only one whose
// conversations may lack a rollout.
const codexTerminalOriginator = "codex-tui"

// codexBackground is a completed request that may need a row of its own.
type codexBackground struct {
	Conversation          string
	AtMS                  int64
	DurationMS, TTFTMs    int64 // 0 when the request record was not seen
	Input, Cached, Output int64
	Model, Effort         string
	Originator            string
	Warmup                bool // never counted elsewhere, whatever the conversation
}

// codexTelemetryEvents picks Codex's request and completion records out of an
// export, in order.
func codexTelemetryEvents(records []telemetryRecord) []codexTelemetryEvent {
	var out []codexTelemetryEvent
	for _, r := range records {
		conv := r["conversation.id"].str()
		at, err := time.Parse(time.RFC3339Nano, r["event.timestamp"].str())
		if conv == "" || err != nil {
			continue
		}
		ev := codexTelemetryEvent{Conversation: conv, AtMS: at.UnixMilli(), Originator: r["originator"].str()}
		switch r["event.name"].str() {
		case "codex.websocket_request":
			if r["success"].falsy() {
				continue
			}
			ev.Request = true
		case "codex.api_request":
			status, hasStatus := r["http.response.status_code"].int()
			if r["endpoint"].str() != "/responses" || r["success"].falsy() || hasStatus && (status < 200 || status > 299) {
				continue
			}
			ev.Request = true
		case "codex.sse_event":
			input, ok := r["input_token_count"].int()
			if r["event.kind"].str() != "response.completed" || !ok {
				continue // the per-event records carry no tokens
			}
			ev.Input = input
			ev.Cached, _ = r["cached_token_count"].int()
			ev.Output, _ = r["output_token_count"].int()
			ev.TTFTMs, ev.HasTTFT = r["ttft_ms"].int()
			ev.Model, ev.Effort = r["model"].str(), r["model_reasoning_effort"].str()
		default:
			continue
		}
		if ev.Request {
			ev.DurationMS, _ = r["duration_ms"].int()
			if ev.DurationMS < 0 || ev.DurationMS > usageTelemetryMaxDuration.Milliseconds() {
				continue
			}
		}
		out = append(out, ev)
	}
	return out
}

// codexPairing remembers, per session and conversation, the request whose
// completion record has not arrived yet: the two usually land in different
// export batches.
type codexPairing struct {
	mu      sync.Mutex
	pending map[string]codexPending
}

type codexPending struct {
	startMS, sendMS int64
	seen            time.Time
}

// feed pairs a session's records. It returns the timings they complete, and
// every completed request as a background candidate: a warm-up always gets a
// row, any other only from a terminal-UI conversation without a rollout.
func (p *codexPairing) feed(sessionID string, evs []codexTelemetryEvent, now time.Time) ([]store.RequestTiming, []codexBackground) {
	if len(evs) == 0 {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[string]codexPending{}
	}
	var out []store.RequestTiming
	var background []codexBackground
	for _, ev := range evs {
		key := sessionID + "\x00" + ev.Conversation
		if ev.Request {
			p.pending[key] = codexPending{startMS: ev.AtMS - ev.DurationMS, sendMS: ev.DurationMS, seen: now}
			continue
		}
		req, started := p.pending[key]
		delete(p.pending, key)
		bg := codexBackground{Conversation: ev.Conversation, AtMS: ev.AtMS, Input: ev.Input, Cached: ev.Cached, Output: ev.Output,
			Model: ev.Model, Effort: ev.Effort, Originator: ev.Originator, Warmup: !ev.HasTTFT && ev.Output == 0}
		dur := ev.AtMS - req.startMS
		timed := started && dur > 0 && dur <= usageTelemetryMaxDuration.Milliseconds()
		if bg.Warmup {
			if timed {
				bg.DurationMS = dur
			}
			background = append(background, bg)
			continue
		}
		if !ev.HasTTFT || ev.TTFTMs < 0 {
			continue // no first output: nothing to show, and not a warm-up
		}
		ttft := ev.TTFTMs + req.sendMS // ttft_ms starts where the request record was written
		if timed && ttft > dur && ttft-dur <= codexClockSlackMS {
			ttft = dur
		}
		if timed && ttft <= dur {
			bg.DurationMS, bg.TTFTMs = dur, ttft
			if id := usage.CodexRequestKey(ev.Conversation, ev.Input, ev.Cached, ev.Output); apiRequestIDRE.MatchString(id) {
				out = append(out, store.RequestTiming{RequestID: id, TTFTMs: ttft, DurationMS: dur})
			}
		}
		background = append(background, bg)
	}
	if len(p.pending) > codexPendingMax/2 {
		for k, v := range p.pending {
			if now.Sub(v.seen) > codexPendingKeep || len(p.pending) > codexPendingMax {
				delete(p.pending, k)
			}
		}
	}
	return out, background
}

// codexRolloutsMax bounds the cache of conversations known to keep a rollout.
const codexRolloutsMax = 8192

// codexRollouts remembers conversations known to keep a rollout, so most
// completions need no directory listing.
type codexRollouts struct {
	mu    sync.Mutex
	known map[string]bool
}

// codexHasRollout reports whether a conversation keeps a session record in the
// session's home. When that cannot be told the answer is yes: a request
// missing from the report beats one counted twice.
func (s *Server) codexHasRollout(sess store.Session, conversation string) bool {
	key := sess.ID + "\x00" + conversation
	c := &s.codexRollouts
	c.mu.Lock()
	known := c.known[key]
	c.mu.Unlock()
	if known {
		return true
	}
	exists, err := s.usageService().CodexRolloutExists(sess, conversation)
	if err != nil {
		return true
	}
	if exists {
		c.mu.Lock()
		if c.known == nil || len(c.known) >= codexRolloutsMax {
			c.known = map[string]bool{}
		}
		c.known[key] = true
		c.mu.Unlock()
	}
	return exists
}

// recordCodexBackground writes the background rows among a batch's completed
// requests. A hidden delegation worker's rows go to the workspace it serves,
// like the rest of its usage.
func (s *Server) recordCodexBackground(sess store.Session, reqs []codexBackground) error {
	if len(reqs) == 0 {
		return nil
	}
	owner := sess
	if id, ok, err := s.store.DelegationOwner(sess.ID); err != nil {
		return err
	} else if ok {
		if o, found := s.store.Get(id); found {
			owner = o
		}
	}
	var evs []store.UsageEvent
	for _, r := range reqs {
		if !r.Warmup && (r.Originator != codexTerminalOriginator || s.codexHasRollout(sess, r.Conversation)) {
			continue
		}
		evs = append(evs, s.usageService().Price(codexBackgroundRow(sess, owner, r)))
	}
	_, err := s.store.InsertBackgroundUsage(evs...)
	return err
}

// codexBackgroundRow builds a background request's row. Its id names the
// request by session, conversation, completion time and tokens: a resent
// export finds it again, two warm-ups of one conversation do not collide.
// A request other than a warm-up also carries its usage.CodexRequestKey, so
// the timing the same export reports for it finds the row already timed
// instead of waiting for one; warm-ups report no timing.
func codexBackgroundRow(src, owner store.Session, r codexBackground) store.UsageEvent {
	id := "codex-telemetry:" + src.ID + ":" + r.Conversation + ":" + strconv.FormatInt(r.AtMS, 10) + ":" +
		strconv.FormatInt(r.Input, 10) + ":" + strconv.FormatInt(r.Output, 10)
	raw, _ := json.Marshal(struct {
		Type         string `json:"type"`
		Conversation string `json:"conversation"`
		Warmup       bool   `json:"warmup,omitempty"`
		Input        int64  `json:"input_tokens"`
		Cached       int64  `json:"cached_input_tokens"`
		Output       int64  `json:"output_tokens"`
	}{"codex.background", r.Conversation, r.Warmup, r.Input, r.Cached, r.Output})
	ev := store.UsageEvent{
		TS: time.UnixMilli(r.AtMS), User: owner.User, SessionID: owner.ID, TurnID: id,
		Agent: config.AgentCodex, AccountID: src.AccountID, Model: r.Model, Kind: store.UsageKindBackground,
		InputTokens: max(r.Input-r.Cached, 0), CacheReadTokens: r.Cached, OutputTokens: r.Output,
		ReqID: id, Raw: string(raw), APITTFTMs: r.TTFTMs, APIDurationMS: r.DurationMS,
	}
	if r.Effort != "" {
		ev.Effort, ev.EffortControl = r.Effort, "effort"
	}
	if key := usage.CodexRequestKey(r.Conversation, r.Input, r.Cached, r.Output); !r.Warmup && apiRequestIDRE.MatchString(key) {
		ev.APIRequestID = key
	}
	return ev
}
