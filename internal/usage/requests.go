package usage

import (
	"encoding/json"
	"time"

	"agentbox/internal/store"
)

// 一行用量 = 一次 provider 请求。回合里的 CLI 每调一次工具就再打一次接口、
// 每次都把整段上下文重发一遍；按回合合并时缓存读取会逐次累加、远超上下文窗口，
// 长上下文档也会按合计错判，所以新记录一律逐次成行：
//
//   - Claude 网页对话：claude_messages.go 按 message.id 去重后每条消息一行；
//   - Codex app-server：收尾事件的 requests（agent/appserver_usage.go 按累计
//     差值得出）在这里展开，每次一行、各自查价判档；
//   - 终端补记：terminal.go / codex_terminal.go 每次请求一行。
//
// 升级前的 Claude 对话行是按回合合并的（raw = claude.messages），账本已按整行
// 扣款，不再拆开；MergedRequests 给出它合并了多少次请求。

type codexRequest struct {
	TS         int64 `json:"ts"`
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens,omitempty"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens,omitempty"`
}

// expandCodexRequests 把带 requests 的 Codex 收尾事件拆成逐次请求的行；没有
// 逐次证据（exec 回退、起标题）时原样返回。回合级耗时只留在第一次请求上。
func expandCodexRequests(ev store.UsageEvent) []store.UsageEvent {
	if ev.Raw == "" || ev.Raw[0] != '{' {
		return []store.UsageEvent{ev}
	}
	var turn struct {
		Type     string         `json:"type"`
		Requests []codexRequest `json:"requests"`
	}
	if json.Unmarshal([]byte(ev.Raw), &turn) != nil || len(turn.Requests) == 0 ||
		turn.Type != "turn.completed" && turn.Type != "turn.failed" {
		return []store.UsageEvent{ev}
	}
	out := make([]store.UsageEvent, 0, len(turn.Requests))
	for i, q := range turn.Requests {
		row := ev
		// 与 ParseUsage 同口径：cached 是 input 的子集，reasoning 已含在 output 里。
		row.InputTokens = max(q.Input-q.Cached, 0)
		row.CacheReadTokens = q.Cached
		row.CacheWriteTokens = q.CacheWrite
		row.OutputTokens = q.Output
		if q.TS != 0 {
			row.TS = time.UnixMilli(q.TS)
		}
		raw, _ := json.Marshal(struct {
			Type string `json:"type"`
			codexRequest
		}{"codex.request", q})
		row.Raw = string(raw)
		if i > 0 {
			row.TTFTMs, row.DurationMS, row.WallMS = 0, 0, 0
		}
		out = append(out, row)
	}
	return out
}

// MergedRequests 是升级前按回合合并的 Claude 对话行里合并了几次请求；逐次成行
// 的新记录和其他来源返回 0。
func MergedRequests(e store.UsageEvent) int {
	t, n := legacyShape(e)
	if t != "claude.messages" {
		return 0
	}
	return n
}

// SingleRequest 报告这一行是不是一次 provider 请求：终端补记行、Codex 逐次行
// （codex.request），以及 raw 是单条 Claude 消息的行。升级前「一个回合 × 一个
// 模型」的合计行（claude.messages、result，以及同回合只在首行存 raw 的其余行）
// 不是，同回合按模型拆出的几行不能标成「第几次 / 共几次」。
func SingleRequest(e store.UsageEvent) bool {
	if e.Kind == store.UsageKindTerminal {
		return true
	}
	if e.Raw == "" || e.Raw[0] != '{' {
		return false
	}
	var r struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal([]byte(e.Raw), &r) != nil {
		return false
	}
	return r.Type == "codex.request" || r.Type == "" && r.ID != ""
}

func legacyShape(e store.UsageEvent) (string, int) {
	if e.Raw == "" || e.Raw[0] != '{' {
		return "", 0
	}
	var r struct {
		Type     string            `json:"type"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal([]byte(e.Raw), &r) != nil {
		return "", 0
	}
	return r.Type, len(r.Messages)
}
