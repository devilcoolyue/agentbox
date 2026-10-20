package agent

import (
	"encoding/json"
	"time"
)

// app-server 的 thread/tokenUsage/updated 带两份数：total 是线程累计，last 是
// 最近一次模型请求。一个回合里每调一次工具就多一次请求，只留最后一份 last
// 会把前面的请求全漏掉（实测 22 次请求的回合只记到最后一次）。而且这条通知
// 在限流信息更新时会原样重发，last 也不能直接相加——同 codex 终端补记，按
// total 的单调差值认出真正新增的那一次请求。

// codexTokens 是 TokenUsageBreakdown 里参与计费的部分。inputTokens 含缓存命中。
type codexTokens struct {
	Input      int64 `json:"inputTokens"`
	Cached     int64 `json:"cachedInputTokens"`
	CacheWrite int64 `json:"cacheWriteInputTokens"`
	Output     int64 `json:"outputTokens"`
	Reasoning  int64 `json:"reasoningOutputTokens"`
}

func (t codexTokens) zero() bool {
	return t.Input == 0 && t.Cached == 0 && t.CacheWrite == 0 && t.Output == 0
}

// covers 判断 t 每个桶都不小于 o：累计值只增不减，倒退说明被压缩/重置过。
func (t codexTokens) covers(o codexTokens) bool {
	return t.Input >= o.Input && t.Cached >= o.Cached && t.CacheWrite >= o.CacheWrite &&
		t.Output >= o.Output && t.Reasoning >= o.Reasoning
}

func (t codexTokens) minus(o codexTokens) codexTokens {
	return codexTokens{t.Input - o.Input, t.Cached - o.Cached, t.CacheWrite - o.CacheWrite, t.Output - o.Output, t.Reasoning - o.Reasoning}
}

func (t codexTokens) negative() bool {
	return t.Input < 0 || t.Cached < 0 || t.CacheWrite < 0 || t.Output < 0 || t.Reasoning < 0
}

// codexRequest 是一次模型请求的用量，字段名与 turn.completed 的 usage 一致
// （input_tokens 同样含缓存命中），TS 是我们收到通知的时刻（Unix 毫秒）。
type codexRequest struct {
	TS         int64 `json:"ts"`
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens,omitempty"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens,omitempty"`
}

// codexMeter 收集本回合的每次请求。
type codexMeter struct {
	total    *codexTokens // 本进程最近一次看到的线程累计，含不属于本回合的通知
	requests []codexRequest
	legacy   *codexTokens // 不带 total 的旧版 app-server：只能退回最近一次 last
}

// observe 处理一条 tokenUsage 通知。turn 是本回合 id（turn/start 返回前为空），
// eventTurn 是通知自带的回合 id（旧版没有）。不属于本回合的通知只更新基线：
// 续聊时恢复出来的累计值不能算成本回合的请求。
func (m *codexMeter) observe(turn, eventTurn string, total, last *codexTokens, at time.Time) {
	mine := turn != "" && (eventTurn == "" || eventTurn == turn)
	if total == nil {
		if mine && last != nil && !last.negative() {
			l := *last
			m.legacy = &l
		}
		return
	}
	if total.negative() {
		return
	}
	prev := m.total
	t := *total
	m.total = &t
	if !mine {
		return
	}
	var d codexTokens
	switch {
	case prev != nil && t.covers(*prev):
		d = t.minus(*prev)
	case last != nil && !last.negative() && t.covers(*last):
		// 本进程第一条，或累计被压缩重置过：只能信 last，且不能超过累计。
		d = *last
	}
	if d.zero() {
		return // 限流更新等重发的同一份累计
	}
	m.requests = append(m.requests, codexRequest{TS: at.UnixMilli(), Input: d.Input, Cached: d.Cached, CacheWrite: d.CacheWrite, Output: d.Output, Reasoning: d.Reasoning})
}

// attach 把本回合用量写进收尾事件：usage 为各次请求之和，requests 逐次列出。
// 中断/失败的回合同样带上——请求已经发生，provider 照样计费。
func (m *codexMeter) attach(ev map[string]any) {
	if len(m.requests) == 0 {
		if m.legacy != nil {
			ev["usage"] = map[string]any{"input_tokens": m.legacy.Input, "cached_input_tokens": m.legacy.Cached, "output_tokens": m.legacy.Output}
		}
		return
	}
	var sum codexRequest
	for _, r := range m.requests {
		sum.Input += r.Input
		sum.Cached += r.Cached
		sum.CacheWrite += r.CacheWrite
		sum.Output += r.Output
		sum.Reasoning += r.Reasoning
	}
	usage := map[string]any{"input_tokens": sum.Input, "cached_input_tokens": sum.Cached, "output_tokens": sum.Output, "reasoning_output_tokens": sum.Reasoning}
	if sum.CacheWrite != 0 {
		usage["cache_write_input_tokens"] = sum.CacheWrite
	}
	ev["usage"] = usage
	ev["requests"] = m.requests
}

// attachUsage 把本回合用量写进收尾事件，有逐次请求时再带上线程 id：用量行按
// 「线程 + 这次请求的 token 数」对上 Codex 遥测报的这一次请求（usage.CodexRequestKey）。
// 回合里压缩过上下文时另带回合 id 与次数：ChatGPT 登录的远程压缩那次请求不在
// tokenUsage 里，结算时按回合 id 去会话记录（rollout 的 compacted）里找它的用量。
func (c *appServerConn) attachUsage(ev map[string]any) {
	c.meter.attach(ev)
	_, requests := ev["requests"]
	if !requests && c.compactions == 0 {
		return
	}
	c.mu.Lock()
	thread, turn := c.thread, c.turn
	c.mu.Unlock()
	if thread != "" {
		ev["thread_id"] = thread
	}
	if c.compactions > 0 && turn != "" {
		ev["turn_id"] = turn
		ev["compactions"] = c.compactions
	}
}

func (c *appServerConn) observeTokenUsage(params json.RawMessage) {
	var p struct {
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Total *codexTokens `json:"total"`
			Last  *codexTokens `json:"last"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.mu.Lock()
	turn := c.turn
	c.mu.Unlock()
	c.meter.observe(turn, p.TurnID, p.TokenUsage.Total, p.TokenUsage.Last, time.Now())
}

// emitContext 把最近一次请求的用量和上下文窗口转成 token_count 事件（与 Codex
// 会话记录里的同名事件同形状），网页据此显示上下文占用，并随历史落盘以便刷新
// 后还原。只在数值变了时发：限流更新会把同一份用量原样重发。
func (c *appServerConn) emitContext(params json.RawMessage) {
	var p struct {
		TokenUsage struct {
			Last *struct {
				Input     int64 `json:"inputTokens"`
				Cached    int64 `json:"cachedInputTokens"`
				Output    int64 `json:"outputTokens"`
				Reasoning int64 `json:"reasoningOutputTokens"`
				Total     int64 `json:"totalTokens"`
			} `json:"last"`
			Window *int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &p) != nil || p.TokenUsage.Last == nil {
		return
	}
	last := p.TokenUsage.Last
	info := map[string]any{"last_token_usage": map[string]any{"input_tokens": last.Input, "cached_input_tokens": last.Cached,
		"output_tokens": last.Output, "reasoning_output_tokens": last.Reasoning, "total_tokens": last.Total}}
	if w := p.TokenUsage.Window; w != nil && *w > 0 {
		info["model_context_window"] = *w
	}
	raw, err := json.Marshal(map[string]any{"type": "token_count", "info": info})
	if err != nil || string(raw) == c.context {
		return
	}
	c.context = string(raw)
	c.emit(raw)
}

// emitPlan 把 update_plan 的计划（turn/plan/updated，每次带完整列表）转成
// codex exec --json 同形状的 todo_list 事件；exec 只有 completed，这里另带
// status 保留「进行中」。
func (c *appServerConn) emitPlan(params json.RawMessage) {
	var p struct {
		Explanation *string `json:"explanation"`
		Plan        []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	items := make([]map[string]any, 0, len(p.Plan))
	for _, step := range p.Plan {
		status := step.Status
		if status == "inProgress" {
			status = "in_progress"
		}
		items = append(items, map[string]any{"text": step.Step, "completed": step.Status == "completed", "status": status})
	}
	item := map[string]any{"type": "todo_list", "items": items}
	if p.Explanation != nil && *p.Explanation != "" {
		item["explanation"] = *p.Explanation
	}
	c.emitJSON(map[string]any{"type": "item.updated", "item": item})
}
