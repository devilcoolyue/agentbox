package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"
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
	timer                        codexTimer
	// lastRequest 是最近一次请求的用量，认出已经进了累计的本地压缩；compacted
	// 是刚从 compacted 记录补过的远程压缩，下一次累计若把它也算进来，不能再记一遍。
	lastRequest *codexTokens
	compacted   *codexTokens
}

// codexTimer 按事件顺序估每次请求的耗时与首字，照 cc-switch 的 RequestTimer
// （session_usage_codex.rs）。rollout 没有请求级计时，各版本的事件顺序也不一样：
//
//   - 结束：0.16x 在响应结束时写 token_usage_record，直接用它。更早的只有
//     token_count，有的版本在响应结束时写，有的要等工具跑完才写；后一种表现为
//     「最后一个输出项之后先出现工具结果，token_count 紧跟着写出」，这时改用
//     最后一个输出项的时刻（略早于真正结束）。
//   - 起点：这次请求第一个输出项之前最近的起点行（上一次的 token_count、工具
//     结果、用户/developer 消息、turn_context）。不取结束前最近的起点行，因为
//     工具结果可能在响应还没结束时就写进来了。
//   - 首字：0.16x 的 item_completed 给思考和回复项记了开始生成的时刻
//     （started_at_ms），取这次请求里最早的一项。一开头就调工具时推不出来。
//
// 旧布局会把上一次响应的输出项补写在它的 token_count 之后（时间戳几乎相同），
// 这些不属于下一次请求，忽略，否则用户隔很久才发下一条时耗时会长得离谱。
// 请求被中断或出错时等不到 token_count，它留下的起点在下一轮开始时清掉。
type codexTimer struct {
	// 都是 Unix 毫秒，0 = 没有。
	lastBoundary, lastTokenCount int64
	requestStart, lastOutput     int64
	toolAfterOutput, firstOutput int64
	record                       int64        // token_usage_record 的时刻
	recordUsage                  *codexTokens // 它记的那次请求的用量
}

// codexSameFlushMS：两行时间戳相差不超过这么多，就算同一批写出的。
const codexSameFlushMS = 100

func (t *codexTimer) boundary(at int64) { t.lastBoundary = max(t.lastBoundary, at) }

func (t *codexTimer) toolOutput(at int64) {
	t.boundary(at)
	if t.lastOutput != 0 {
		t.toolAfterOutput = at
	}
}

func (t *codexTimer) modelOutput(at int64) {
	if t.requestStart == 0 && t.lastTokenCount != 0 && at-t.lastTokenCount <= codexSameFlushMS {
		return // 上一次响应补写在 token_count 之后的输出项
	}
	if t.requestStart == 0 {
		t.requestStart = t.lastBoundary
	}
	t.lastOutput, t.toolAfterOutput = at, 0
}

func (t *codexTimer) outputStarted(at int64) {
	if t.firstOutput == 0 || at < t.firstOutput {
		t.firstOutput = at
	}
}

// reset：一轮开始或被中断，没等到 token_count 的那次请求作废。
func (t *codexTimer) reset(at int64) {
	*t = codexTimer{lastBoundary: max(t.lastBoundary, at), lastTokenCount: t.lastTokenCount}
}

// finish 在一次有新用量的 token_count 上结算这次请求，并把它记成下一次请求的
// 起点。last 是这次请求自己的用量，用来确认 token_usage_record 说的是同一次。
func (t *codexTimer) finish(at int64, last *codexTokens) (wall, ttft int64) {
	start := t.requestStart
	if start == 0 {
		start = t.lastBoundary
	}
	end := at
	if t.toolAfterOutput != 0 && (at == 0 || at-t.toolAfterOutput <= codexSameFlushMS) {
		end = t.lastOutput // token_count 等工具跑完才写
	}
	if u := t.recordUsage; u != nil && (last == nil || (u.Input == last.Input && u.Cached == last.Cached && u.Output == last.Output)) {
		end = t.record
	}
	first := t.firstOutput
	*t = codexTimer{lastBoundary: max(t.lastBoundary, at), lastTokenCount: at}
	return termTiming(start, first, end)
}

// observe 把与计时有关的行交给 timer。时间戳解析不了的行不参与计时。
func (p *codexTermParser) observe(kind string, payload json.RawMessage, at int64) {
	if at == 0 {
		return
	}
	switch kind {
	case "turn_context":
		p.timer.boundary(at)
	case "token_usage_record":
		var rec struct {
			Usage *codexTokens `json:"usage"`
		}
		_ = json.Unmarshal(payload, &rec)
		p.timer.record, p.timer.recordUsage = at, rec.Usage
	case "response_item":
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(payload, &item) != nil {
			return
		}
		switch {
		case item.Type == "message" && item.Role == "assistant", item.Type == "reasoning", strings.HasSuffix(item.Type, "_call"):
			p.timer.modelOutput(at)
		case item.Type == "message":
			p.timer.boundary(at)
		case strings.HasSuffix(item.Type, "_output"):
			p.timer.toolOutput(at)
		}
	case "event_msg":
		var msg struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
			StartedAtMs int64 `json:"started_at_ms"`
		}
		if json.Unmarshal(payload, &msg) != nil {
			return
		}
		switch msg.Type {
		case "task_started", "turn_started", "turn_aborted":
			p.timer.reset(at)
		case "item_completed":
			// 只看模型输出的思考与回复；工具执行、用户消息不是模型在输出。
			if (msg.Item.Type == "Reasoning" || msg.Item.Type == "AgentMessage") && msg.StartedAtMs > 0 {
				p.timer.outputStarted(msg.StartedAtMs)
			}
		}
	}
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
	p.observe(rec.Type, rec.Payload, termMillis(rec.Timestamp))
	switch rec.Type {
	case "compacted":
		p.compaction(rec.Payload, ts)
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

// compaction 记下 ChatGPT 登录远程压缩的那次请求（见 codex_compaction.go）：
// 与紧挨着的上一次请求相同就是已进累计的本地压缩，不再记。
func (p *codexTermParser) compaction(payload json.RawMessage, ts time.Time) {
	var rec codexCompactionRecord
	if json.Unmarshal(payload, &rec) != nil {
		return
	}
	turn, response, u, ok := rec.usage()
	if !ok || p.lastRequest != nil && sameRequest(*p.lastRequest, u) {
		return
	}
	p.compacted = &u
	if turn == "" {
		turn = p.turn
	}
	if !p.allowed || p.session == "" || turn == "" {
		return
	}
	if response == "" {
		response = ts.Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte(p.session + "\x00compaction\x00" + response))
	turnSum := sha256.Sum256([]byte(p.session + "\x00" + turn))
	row := termTurn{reqID: "codex-terminal:" + hex.EncodeToString(sum[:]), model: p.model, ts: ts,
		ev: store.UsageEvent{TurnID: "codex-terminal:" + hex.EncodeToString(turnSum[:]), Effort: p.effort}}
	if p.effort != "" {
		row.ev.EffortControl = "effort"
	}
	row.ev.InputTokens = max(u.Input-u.Cached, 0)
	row.ev.CacheReadTokens = u.Cached
	row.ev.OutputTokens = u.Output
	row.ev.CacheWriteTokens = u.CacheWrite
	row.ev.APIRequestID = CodexRequestKey(p.session, u.Input, u.Cached, u.Output)
	p.out = append(p.out, row)
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
	if delta.Input < 0 || delta.Cached < 0 || delta.Output < 0 || delta.CacheWrite < 0 {
		return
	}
	if delta.Input == 0 && delta.Cached == 0 && delta.Output == 0 && delta.CacheWrite == 0 {
		return // 限额刷新时重发的快照不是一次请求，也不参与计时
	}
	if c := p.compacted; c != nil {
		p.compacted = nil
		if sameRequest(delta, *c) {
			p.lastRequest = &delta
			return // 补过的压缩请求后来进了累计
		}
		if l := info.Last; l != nil && !sameRequest(delta, *l) && delta.Input >= l.Input && delta.Cached >= l.Cached &&
			delta.Output >= l.Output && delta.CacheWrite >= l.CacheWrite && (l.Input > 0 || l.Output > 0) {
			delta = *l // 累计里多出来的是那次压缩
		}
	}
	p.lastRequest = &delta
	var at int64
	if !ts.IsZero() {
		at = ts.UnixMilli()
	}
	wall, ttft := p.timer.finish(at, info.Last)
	if !p.allowed || p.session == "" || p.turn == "" {
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
	row := termTurn{reqID: id, model: p.model, ts: ts, ev: store.UsageEvent{TurnID: "codex-terminal:" + hex.EncodeToString(turnSum[:]), Effort: p.effort, WallMS: wall, TTFTMs: ttft}}
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
	// 遥测里的实测计时按会话 + 这次请求的 token 数对上这一行。
	row.ev.APIRequestID = CodexRequestKey(p.session, delta.Input, delta.Cached, delta.Output)
	p.out = append(p.out, row)
}
