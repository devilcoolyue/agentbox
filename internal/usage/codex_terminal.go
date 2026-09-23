package usage

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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

// parseCodexTerminal reads only codex-tui rollouts. token_count is emitted again
// on rate-limit updates, so last_token_usage must never be blindly summed.
// Monotonic total_token_usage deltas identify actual new usage; last is used
// only at an initial observation or counter reset, bounded by the new total.
func parseCodexTerminal(f io.Reader) ([]termTurn, error) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	session, turn, model := "", "", ""
	allowed := false
	var previous codexTokens
	havePrevious := false
	acc := map[string]*termTurn{}
	var order []string
	for sc.Scan() {
		var rec struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
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
				continue
			}
			allowed = meta.Originator == "codex-tui" && (len(meta.Source) == 0 || string(meta.Source) == `"cli"` || string(meta.Source) == `"tui"`)
			session = meta.ID
		case "turn_context":
			var ctx struct {
				ID    string `json:"turn_id"`
				Model string `json:"model"`
			}
			if json.Unmarshal(rec.Payload, &ctx) != nil {
				continue
			}
			if ctx.ID != "" {
				turn = ctx.ID
			}
			model = ctx.Model
		case "event_msg":
			var msg struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
				Info   *struct {
					Total *codexTokens `json:"total_token_usage"`
					Last  *codexTokens `json:"last_token_usage"`
				} `json:"info"`
			}
			if json.Unmarshal(rec.Payload, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "task_started", "turn_started":
				turn = msg.TurnID
			case "task_complete", "turn_complete", "turn_aborted":
				turn = ""
			case "token_count":
				if msg.Info == nil || msg.Info.Total == nil {
					continue
				}
				total := *msg.Info.Total
				if total.Input < 0 || total.Cached < 0 || total.Output < 0 || total.CacheWrite < 0 {
					continue
				}
				var delta codexTokens
				if havePrevious && total.Input >= previous.Input && total.Cached >= previous.Cached && total.Output >= previous.Output && total.CacheWrite >= previous.CacheWrite {
					delta = codexTokens{Input: total.Input - previous.Input, Cached: total.Cached - previous.Cached, Output: total.Output - previous.Output, CacheWrite: total.CacheWrite - previous.CacheWrite}
				} else if msg.Info.Last != nil {
					delta = *msg.Info.Last
					// Reset/compaction estimates can replace totals with a synthetic count;
					// don't bill a delta that the cumulative token buckets cannot support.
					if delta.Input > total.Input || delta.Cached > total.Cached || delta.Output > total.Output || delta.CacheWrite > total.CacheWrite {
						delta = codexTokens{}
					}
				}
				previous = total
				havePrevious = true
				if !allowed || session == "" || turn == "" || delta.Input < 0 || delta.Cached < 0 || delta.Output < 0 || delta.CacheWrite < 0 {
					continue
				}
				if delta.Input == 0 && delta.Cached == 0 && delta.Output == 0 && delta.CacheWrite == 0 {
					continue
				}
				key := session + "\x00" + turn + "\x00" + model
				row := acc[key]
				if row == nil {
					sum := sha256.Sum256([]byte(key))
					id := "codex-terminal:" + hex.EncodeToString(sum[:])
					turnSum := sha256.Sum256([]byte(session + "\x00" + turn))
					row = &termTurn{reqID: id, model: model, ts: ts, ev: store.UsageEvent{TurnID: "codex-terminal:" + hex.EncodeToString(turnSum[:])}}
					acc[key] = row
					order = append(order, key)
				}
				uncached := delta.Input - delta.Cached
				if uncached < 0 {
					uncached = 0
				}
				row.ev.InputTokens += uncached
				row.ev.CacheReadTokens += delta.Cached
				row.ev.OutputTokens += delta.Output
				row.ev.CacheWriteTokens += delta.CacheWrite
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	rows := make([]termTurn, 0, len(order))
	for _, key := range order {
		rows = append(rows, *acc[key])
	}
	return rows, nil
}
