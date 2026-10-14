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
