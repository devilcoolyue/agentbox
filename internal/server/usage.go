package server

// 用量落库：把 provider 在回合收尾时报的 token 与费用写进 usage_events。
//
// 现阶段只记录、不扣额度——计费模型还没定，但用量数据不等人：不记的话这段
// 时间的真实消耗永久缺失，将来做报表没有历史可回溯。扣减逻辑（事务、幂等、
// 超额策略）是后一步的事，那时直接读这张表即可。
//
// 两种 agent 报的形状完全不同：
//
//   claude  {"type":"result", "total_cost_usd":…, "duration_ms":…,
//            "usage":{…}, "modelUsage":{"<model>":{…}}}
//   codex   {"type":"turn.completed", "usage":{input_tokens, cached_input_tokens,
//            output_tokens}}
//
// claude 的 modelUsage 按模型分列，且包含子 agent 用掉的模型（起标题的 haiku
// 等），实测 total_cost_usd 恰好等于各模型 costUSD 之和，而顶层 usage 只覆盖
// 主模型。计费必须以 modelUsage 为准，拿顶层 usage 会漏记。

import (
	"encoding/json"
	"log"
	"math"
	"sort"
	"time"

	"agentbox/internal/store"
)

// claudeResult 是 claude stream-json 的收尾事件里与计费相关的部分。
type claudeResult struct {
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	Usage      struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		Input      int64   `json:"inputTokens"`
		Output     int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		CostUSD    float64 `json:"costUSD"`
	} `json:"modelUsage"`
}

// codexTurn 是 codex 的回合收尾事件。app-server 路径由 appserver.go 翻译产出，
// exec --json 回退路径由 codex 自己产出——实测（codex-cli 0.145.0）两者形状一致。
type codexTurn struct {
	Usage struct {
		Input      int64 `json:"input_tokens"`
		Cached     int64 `json:"cached_input_tokens"`
		CacheWrite int64 `json:"cache_write_input_tokens"`
		Output     int64 `json:"output_tokens"`
		// Reasoning 已经含在 Output 里，实测同一回合 output=150 / reasoning=143
		// 而答案本身只有两三个 token。单独累加会把思考部分算两遍，所以这里
		// 只解析出来占位、不参与计算。
		Reasoning int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
}

// microUSD 把 provider 报的美元浮点转成整数微美元。四舍五入的误差在 1e-6
// 美元以内且确定，换来的是报表求和不再受 float 累积误差影响。
func microUSD(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

// parseUsage 把一行回合收尾事件翻成用量流水；不是用量事件时返回 nil。
// base 提供归属信息（用户/会话/线程/回合/agent/账号，以及回合请求的模型）,
// 其余字段由事件本身填。纯函数，便于直接对着真实事件样本测试。
func parseUsage(line []byte, base store.UsageEvent) []store.UsageEvent {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return nil
	}

	switch probe.Type {
	case "result":
		var r claudeResult
		if json.Unmarshal(line, &r) != nil {
			return nil
		}
		base.DurationMS = r.DurationMS

		// 无 modelUsage（较旧的 claude）时退回顶层 usage，模型记为本回合请求
		// 的那个；此时子 agent 的消耗确实统计不到，属已知精度损失。
		if len(r.ModelUsage) == 0 {
			u := r.Usage
			if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
				return nil
			}
			ev := base
			ev.InputTokens, ev.OutputTokens = u.Input, u.Output
			ev.CacheReadTokens, ev.CacheWriteTokens = u.CacheRead, u.CacheWrite
			ev.CostMicroUSD = microUSD(r.CostUSD)
			ev.Raw = string(line)
			return []store.UsageEvent{ev}
		}

		// 按模型名排序，保证同一回合的行顺序稳定（测试与人工核对都靠这个）。
		models := make([]string, 0, len(r.ModelUsage))
		for m := range r.ModelUsage {
			models = append(models, m)
		}
		sort.Strings(models)

		out := make([]store.UsageEvent, 0, len(models))
		for i, m := range models {
			u := r.ModelUsage[m]
			ev := base
			ev.Model = m
			ev.InputTokens, ev.OutputTokens = u.Input, u.Output
			ev.CacheReadTokens, ev.CacheWriteTokens = u.CacheRead, u.CacheWrite
			ev.CostMicroUSD = microUSD(u.CostUSD)
			if i == 0 { // 原始事件整回合存一份就够，避免每行重复几 KB
				ev.Raw = string(line)
			}
			out = append(out, ev)
		}
		return out

	case "turn.completed":
		var t codexTurn
		if json.Unmarshal(line, &t) != nil {
			return nil
		}
		u := t.Usage
		if u.Input == 0 && u.Output == 0 && u.Cached == 0 {
			return nil
		}
		// codex 的 cached_input_tokens 是 input_tokens 的子集，减掉才能和 claude
		// 那侧的「未命中缓存的输入」对齐。实测确认：同一账号连跑两回合，第二回合
		// 多喂约 2000 token 填充后 input 10434→12455 而 cached 恒为 8576；若 input
		// 只计未命中部分，仅六个 token 的用户输入不可能得出 10434。
		ev := base
		ev.InputTokens = u.Input - u.Cached
		if ev.InputTokens < 0 {
			ev.InputTokens = 0
		}
		ev.CacheReadTokens = u.Cached
		ev.CacheWriteTokens = u.CacheWrite
		ev.OutputTokens = u.Output // 已含 reasoning，勿再加
		// codex 不报费用，留 0，等定价表就位后按 token 折算。
		ev.Raw = string(line)
		return []store.UsageEvent{ev}
	}
	return nil
}

// recordUsage 从一行完整事件里提取用量、按需定价并落库，返回写进去的行数。
// 落库同时会从用户额度里扣掉这笔钱（store.InsertUsage 同事务完成）。尽力而为：
// 解析或写库失败只记日志，绝不影响正在进行的对话。
func (r *chatRoom) recordUsage(base store.UsageEvent, line []byte) int {
	evs := parseUsage(line, base)
	if len(evs) == 0 {
		return 0
	}
	now := time.Now()
	for i := range evs {
		evs[i].TS = now
		// provider 不报价的行（codex）在这里按配置的价目表折算，落库前定好价，
		// 扣减才有依据。查不到价就是 0，只记不扣。
		evs[i].CostMicroUSD = r.srv.priceEvent(evs[i])
	}
	if err := r.srv.store.InsertUsage(evs...); err != nil {
		log.Printf("record usage %s: %v", base.SessionID, err)
		return 0
	}
	return len(evs)
}
