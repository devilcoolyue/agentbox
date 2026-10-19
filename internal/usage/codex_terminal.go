package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"agentbox/internal/store"
)

type codexTokens struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
	Total      int64 `json:"total_tokens"`
}

type codexTokenInfo struct {
	Total *codexTokens `json:"total_token_usage"`
	Last  *codexTokens `json:"last_token_usage"`
}

// parseCodexTerminal reads only codex-tui rollouts. token_count is emitted again
// on rate-limit updates, so last_token_usage must never be blindly summed.
// Monotonic total_token_usage deltas identify actual new usage; last is used
// only at an initial observation or counter reset, bounded by the new total.
//
// Each nonzero delta is one provider request and becomes its own row. Older
// versions merged a turn/model into one row keyed by its hash; the first
// request keeps that key (so a rescan updates the merged row in place) and
// later requests append their sequence number.
func parseCodexTerminal(f io.Reader) ([]termTurn, error) {
	p := newCodexTermParser()
	if err := feedTranscript(f, p); err != nil {
		return nil, err
	}
	return p.rows(), nil
}

// codexTermParser carries parseCodexTerminal's state from line to line, so the
// scanner can continue a rollout where it stopped instead of rereading it.
type codexTermParser struct {
	session, turn, model, effort string
	allowed                      bool
	previous                     codexTokens
	havePrevious                 bool
	seq                          map[string]int
	out                          []termTurn
}

// codexTerminalSource reports whether a codex-tui rollout's source is the TUI's
// own session. Up to 0.154 the TUI wrote "cli"; since 0.160 it runs on the
// app-server and writes "vscode" (the app-server's default). The originator
// still tells them apart: agentbox's own app-server sessions are "agentbox" and
// exec is "codex_exec". Subagent sources are objects and stay excluded.
func codexTerminalSource(source json.RawMessage) bool {
	switch string(source) {
	case "", `"cli"`, `"tui"`, `"vscode"`:
		return true
	}
	return false
}

func newCodexTermParser() *codexTermParser {
	return &codexTermParser{seq: map[string]int{}}
}

func (p *codexTermParser) rows() []termTurn { return p.out }

func (p *codexTermParser) line(line []byte) {
	var rec struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
	switch rec.Type {
	case "session_meta":
		var meta struct {
			ID         string          `json:"id"`
			Originator string          `json:"originator"`
			Source     json.RawMessage `json:"source"`
		}
		if json.Unmarshal(rec.Payload, &meta) != nil {
			return
		}
		p.allowed = meta.Originator == "codex-tui" && codexTerminalSource(meta.Source)
		p.session = meta.ID
	case "turn_context":
		var ctx struct {
			ID     string          `json:"turn_id"`
			Model  string          `json:"model"`
			Effort json.RawMessage `json:"effort"`
		}
		if json.Unmarshal(rec.Payload, &ctx) != nil {
			return
		}
		if ctx.ID != "" {
			p.turn = ctx.ID
		}
		p.model = ctx.Model
		p.effort = transcriptEffort(ctx.Effort)
	case "event_msg":
		var msg struct {
			Type   string          `json:"type"`
			TurnID string          `json:"turn_id"`
			Info   *codexTokenInfo `json:"info"`
		}
		if json.Unmarshal(rec.Payload, &msg) != nil {
			return
		}
		switch msg.Type {
		case "task_started", "turn_started":
			// Some rollouts can persist the context before the start marker;
			// keep it only when it explicitly belongs to this same turn.
			if p.turn != msg.TurnID {
				p.effort = ""
			}
			p.turn = msg.TurnID
		case "task_complete", "turn_complete", "turn_aborted":
			p.turn = ""
			p.effort = ""
		case "token_count":
			p.tokenCount(msg.Info, ts)
		}
	}
}

func (p *codexTermParser) tokenCount(info *codexTokenInfo, ts time.Time) {
	if info == nil || info.Total == nil {
		return
	}
	total := *info.Total
	if total.Input < 0 || total.Cached < 0 || total.Output < 0 || total.CacheWrite < 0 {
		return
	}
	var delta codexTokens
	previous := p.previous
	if p.havePrevious && total.Input >= previous.Input && total.Cached >= previous.Cached && total.Output >= previous.Output && total.CacheWrite >= previous.CacheWrite {
		delta = codexTokens{Input: total.Input - previous.Input, Cached: total.Cached - previous.Cached, Output: total.Output - previous.Output, CacheWrite: total.CacheWrite - previous.CacheWrite}
	} else if info.Last != nil {
		delta = *info.Last
		// Reset/compaction estimates can replace totals with a synthetic count;
		// don't bill a delta that the cumulative token buckets cannot support.
		if delta.Input > total.Input || delta.Cached > total.Cached || delta.Output > total.Output || delta.CacheWrite > total.CacheWrite {
			delta = codexTokens{}
		}
	}
	p.previous = total
	p.havePrevious = true
	if !p.allowed || p.session == "" || p.turn == "" || delta.Input < 0 || delta.Cached < 0 || delta.Output < 0 || delta.CacheWrite < 0 {
		return
	}
	if delta.Input == 0 && delta.Cached == 0 && delta.Output == 0 && delta.CacheWrite == 0 {
		return
	}
	key := p.session + "\x00" + p.turn + "\x00" + p.model
	sum := sha256.Sum256([]byte(key))
	id := "codex-terminal:" + hex.EncodeToString(sum[:])
	if n := p.seq[key]; n > 0 {
		id += ":" + strconv.Itoa(n)
	}
	p.seq[key]++
	turnSum := sha256.Sum256([]byte(p.session + "\x00" + p.turn))
	row := termTurn{reqID: id, model: p.model, ts: ts, ev: store.UsageEvent{TurnID: "codex-terminal:" + hex.EncodeToString(turnSum[:]), Effort: p.effort}}
	if p.effort != "" {
		row.ev.EffortControl = "effort"
	}
	uncached := delta.Input - delta.Cached
	if uncached < 0 {
		uncached = 0
	}
	row.ev.InputTokens = uncached
	row.ev.CacheReadTokens = delta.Cached
	row.ev.OutputTokens = delta.Output
	row.ev.CacheWriteTokens = delta.CacheWrite
	p.out = append(p.out, row)
}
