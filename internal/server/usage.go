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
//
// 一个回合不保证只有一个收尾事件，而 claude 的 result 报的是累计值——两件事
// 撞在一起就是重复扣款，见 usageTally。

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
		// Provider 是 claude 自己说这一段谁服务的（实测 "firstParty"）。存下来
		// 是为了在使用记录里一眼分清官方直连与中转站，别的地方不依赖它。
		Provider string `json:"provider"`
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
//
// cumulative 说明这些数是「本次进程运行至今的累计值」（claude）还是「本回合
// 自己的增量」（codex）——同一回合收到多个收尾事件时，前者只能取最后一个，
// 后者才该相加。见 usageTally。
func parseUsage(line []byte, base store.UsageEvent) (evs []store.UsageEvent, cumulative bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return nil, false
	}

	switch probe.Type {
	case "result":
		var r claudeResult
		if json.Unmarshal(line, &r) != nil {
			return nil, false
		}
		base.DurationMS = r.DurationMS

		// 无 modelUsage（较旧的 claude）时退回顶层 usage，模型记为本回合请求
		// 的那个；此时子 agent 的消耗确实统计不到，属已知精度损失。
		if len(r.ModelUsage) == 0 {
			u := r.Usage
			if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
				return nil, false
			}
			ev := base
			ev.InputTokens, ev.OutputTokens = u.Input, u.Output
			ev.CacheReadTokens, ev.CacheWriteTokens = u.CacheRead, u.CacheWrite
			ev.CostMicroUSD = microUSD(r.CostUSD)
			ev.Raw = string(line)
			return []store.UsageEvent{ev}, true
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
			ev.Provider = u.Provider
			if i == 0 { // 原始事件整回合存一份就够，避免每行重复几 KB
				ev.Raw = string(line)
			}
			out = append(out, ev)
		}
		return out, true

	case "turn.completed":
		var t codexTurn
		if json.Unmarshal(line, &t) != nil {
			return nil, false
		}
		u := t.Usage
		if u.Input == 0 && u.Output == 0 && u.Cached == 0 {
			return nil, false
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
		return []store.UsageEvent{ev}, false
	}
	return nil, false
}

// isOutputEvent 判断一行事件是不是「模型开始出东西了」，用来量首字延迟。
//
// provider 不报这个指标，只能自己掐表。起点取容器就绪之后（见 runTurn），所以
// 量到的是 CLI 启动 + 组上下文 + 模型首个 token 的总和，不是纯粹的模型延迟；
// 对「这回合卡不卡」这个问题来说这才是用户感觉到的那个数。
//
// 各家最早的输出事件形状不同：claude 是增量 stream_event、随后完整 assistant；
// codex（app-server 与 exec 两条路径）是 stream_event 与 item.*。会话初始化事件
// （claude 的 system/init、codex 的 thread.started）不算——它们在模型被调用之前
// 就发出来了，认了就会把首字延迟量成一个恒定的小数字。
func isOutputEvent(line []byte) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return false
	}
	switch ev.Type {
	case "stream_event", "assistant", "item.started", "item.completed":
		return true
	}
	return false
}

// usageTally 把一个回合里的所有收尾事件收敛成一份账，回合结束时一次落库。
//
// 一次 claude 进程运行会吐出不止一个 result：每个子代理跑完补一个完成通知
// （origin.kind = task-notification），撞上 429 每次重试也补一个报错的。而
// 它们带的 modelUsage 是本次运行至今的累计值，不是各自的增量——线上抓到过
// 同一回合 14 个 result，modelUsage 一模一样（fable $25.8674），只有
// total_cost_usd 从 $12.6 递增到 $25.9。当时逐个记账，同一笔钱扣了 14 遍。
//
// 所以累计式事件（claude result）后一个覆盖前一个，只认最后那个总数；增量式
// 事件（codex turn.completed）才相加。
type usageTally struct {
	evs []store.UsageEvent
}

// observe 把一行事件并进汇总；不是用量事件就什么也不做。
func (t *usageTally) observe(base store.UsageEvent, line []byte) {
	evs, cumulative := parseUsage(line, base)
	if len(evs) == 0 {
		return
	}
	if cumulative {
		t.evs = evs
		return
	}
	t.evs = append(t.evs, evs...)
}

// flush 把汇总好的用量定价后落库，返回写进去的行数。落库同时会从用户额度里扣
// 掉这笔钱（store.InsertUsage 同事务完成）。尽力而为：写库失败只记日志，绝不
// 影响正在进行的对话。汇总为空（没认出用量事件）时返回 0。
//
// wall 是本回合在我们这边的墙钟耗时，只有到这里回合才真的结束、才量得到，所以
// 由调用方在 flush 的时刻算好传进来（见 chat.go 的 defer）。0 表示没量。
func (r *chatRoom) flushUsage(t *usageTally, wall time.Duration) int {
	if len(t.evs) == 0 {
		return 0
	}
	evs := t.evs
	t.evs = nil // 防重复落库：同一个 tally 再 flush 一次不该再扣钱
	now := time.Now()
	for i := range evs {
		evs[i].TS = now
		evs[i].WallMS = wall.Milliseconds()
		// provider 不报价的行（codex）在这里按配置的价目表折算，落库前定好价，
		// 扣减才有依据。查不到价就是 0，只记不扣。
		evs[i].CostMicroUSD = r.srv.priceEvent(evs[i])
	}
	if err := r.srv.store.InsertUsage(evs...); err != nil {
		log.Printf("record usage %s: %v", evs[0].SessionID, err)
		return 0
	}
	return len(evs)
}

// recordUsage 记一趟自成一体的消耗（起标题）：只有一行收尾事件，收下就落库。
func (r *chatRoom) recordUsage(base store.UsageEvent, line []byte, wall time.Duration) int {
	var t usageTally
	t.observe(base, line)
	return r.flushUsage(&t, wall)
}
