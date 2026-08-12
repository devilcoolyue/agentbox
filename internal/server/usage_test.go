package server

import (
	"testing"

	"agentbox/internal/store"
)

// 下面的事件样本取自生产 transcript（data/users/*/chats/*.jsonl），数值原样
// 保留，只删掉与计费无关的大字段。改动解析逻辑时请对着真实形状校验。

// claude 一个回合用了两个模型：主模型 opus 加上子 agent 的 haiku。
const claudeResultSample = `{"type":"result","subtype":"success","duration_ms":19788,"num_turns":1,` +
	`"total_cost_usd":0.103702,` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":5265,"cache_read_input_tokens":7342,"output_tokens":1870,"service_tier":"standard"},` +
	`"modelUsage":{` +
	`"claude-haiku-4-5-20251001":{"inputTokens":531,"outputTokens":18,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.000621},` +
	`"claude-opus-4-8":{"inputTokens":2,"outputTokens":1870,"cacheReadInputTokens":7342,"cacheCreationInputTokens":5265,"costUSD":0.103081}}}`

// app-server 路径（appserver.go 翻译产出）的形状。
const codexTurnSample = `{"type":"turn.completed","usage":{"cached_input_tokens":3840,"input_tokens":16949,"output_tokens":14}}`

// exec --json 路径的形状，取自容器里对 codex-cli 0.145.0 的实测：多两个字段，
// 事件类型与结构和 app-server 那侧一致。这一条 output=150 / reasoning=143，
// 而模型答案只有三个字符——用来锁住「reasoning 已含在 output 里」。
const codexExecSample = `{"type":"turn.completed","usage":{"input_tokens":12455,` +
	`"cached_input_tokens":8576,"cache_write_input_tokens":0,` +
	`"output_tokens":150,"reasoning_output_tokens":143}}`

func baseEvent() store.UsageEvent {
	return store.UsageEvent{
		User: "boxadmin", SessionID: "sess1", ThreadID: "th1", TurnID: "turn1",
		Agent: "claude", AccountID: "acct1", Model: "requested-model",
	}
}

func TestParseUsageClaudePerModel(t *testing.T) {
	evs, _ := parseUsage([]byte(claudeResultSample), baseEvent())
	if len(evs) != 2 {
		t.Fatalf("每个用到的模型应各出一行，得到 %d 行", len(evs))
	}

	// 排序保证顺序稳定：haiku 在前，opus 在后。
	if evs[0].Model != "claude-haiku-4-5-20251001" || evs[1].Model != "claude-opus-4-8" {
		t.Fatalf("模型顺序不稳定: %q, %q", evs[0].Model, evs[1].Model)
	}

	// 子 agent 的 haiku 必须记全：顶层 usage 里没有它，漏了就等于漏计费。
	if evs[0].InputTokens != 531 || evs[0].OutputTokens != 18 {
		t.Errorf("haiku token 数错: %+v", evs[0])
	}
	if evs[0].CostMicroUSD != 621 {
		t.Errorf("haiku 费用应为 621 微美元，得到 %d", evs[0].CostMicroUSD)
	}

	o := evs[1]
	if o.InputTokens != 2 || o.OutputTokens != 1870 ||
		o.CacheReadTokens != 7342 || o.CacheWriteTokens != 5265 {
		t.Errorf("opus token 数错: %+v", o)
	}
	if o.CostMicroUSD != 103081 {
		t.Errorf("opus 费用应为 103081 微美元，得到 %d", o.CostMicroUSD)
	}

	// 整数微美元求和必须精确等于 provider 报的 total_cost_usd。
	// 单次相加 float64 也算得准，但报表是成千上万行累加，那里 float 会漂：
	// 0.000621 连加一千次得到 0.6209999999999912 而非 0.621。整数不会。
	var total int64
	for _, e := range evs {
		total += e.CostMicroUSD
	}
	if total != 103702 {
		t.Errorf("费用合计应为 103702 微美元（total_cost_usd 0.103702），得到 %d", total)
	}

	// 归属信息逐行带上，raw 只存一份。
	for i, e := range evs {
		if e.TurnID != "turn1" || e.User != "boxadmin" || e.ThreadID != "th1" {
			t.Errorf("第 %d 行归属信息丢失: %+v", i, e)
		}
		if e.DurationMS != 19788 {
			t.Errorf("第 %d 行 duration 应为回合级 19788，得到 %d", i, e.DurationMS)
		}
	}
	if evs[0].Raw == "" {
		t.Error("首行应留存原始事件")
	}
	if evs[1].Raw != "" {
		t.Error("同一回合的后续行不应重复存原始事件")
	}
}

// 旧版 claude 没有 modelUsage 时退回顶层 usage。
func TestParseUsageClaudeNoModelUsage(t *testing.T) {
	const legacy = `{"type":"result","duration_ms":1000,"total_cost_usd":0.5,` +
		`"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":40}}`
	evs, _ := parseUsage([]byte(legacy), baseEvent())
	if len(evs) != 1 {
		t.Fatalf("应回退成单行，得到 %d 行", len(evs))
	}
	e := evs[0]
	if e.InputTokens != 10 || e.CacheWriteTokens != 20 || e.CacheReadTokens != 30 || e.OutputTokens != 40 {
		t.Errorf("顶层 usage 映射错: %+v", e)
	}
	if e.CostMicroUSD != 500000 {
		t.Errorf("费用应为 500000 微美元，得到 %d", e.CostMicroUSD)
	}
	if e.Model != "requested-model" {
		t.Errorf("无 modelUsage 时应记回合请求的模型，得到 %q", e.Model)
	}
}

func TestParseUsageCodex(t *testing.T) {
	base := baseEvent()
	base.Agent = "codex"
	base.Model = "" // codex 常走 provider 默认模型，事件里也不带模型名
	evs, _ := parseUsage([]byte(codexTurnSample), base)
	if len(evs) != 1 {
		t.Fatalf("应出 1 行，得到 %d 行", len(evs))
	}
	e := evs[0]
	// codex 的 cached 是 input 的子集，减掉后与 claude 的「未命中缓存输入」同义。
	if e.InputTokens != 16949-3840 {
		t.Errorf("未命中缓存的输入应为 %d，得到 %d", 16949-3840, e.InputTokens)
	}
	if e.CacheReadTokens != 3840 || e.OutputTokens != 14 {
		t.Errorf("codex token 数错: %+v", e)
	}
	if e.CostMicroUSD != 0 {
		t.Errorf("codex 不报费用，应留 0 待按 token 定价，得到 %d", e.CostMicroUSD)
	}
	if e.Raw == "" {
		t.Error("codex 行应留存原始事件，供日后按 raw 重算")
	}
}

// exec --json 回退路径：形状与 app-server 一致，多出的两个字段不能算错账。
// 尤其是 reasoning——它已经含在 output_tokens 里，再加一遍等于按 293 收钱。
func TestParseUsageCodexExec(t *testing.T) {
	base := baseEvent()
	base.Agent = "codex"
	base.Model = ""
	evs, _ := parseUsage([]byte(codexExecSample), base)
	if len(evs) != 1 {
		t.Fatalf("应出 1 行，得到 %d 行", len(evs))
	}
	e := evs[0]
	if e.InputTokens != 12455-8576 {
		t.Errorf("未命中缓存的输入应为 %d，得到 %d", 12455-8576, e.InputTokens)
	}
	if e.CacheReadTokens != 8576 {
		t.Errorf("cache read = %d，想要 8576", e.CacheReadTokens)
	}
	if e.CacheWriteTokens != 0 {
		t.Errorf("cache write = %d，事件里报的是 0", e.CacheWriteTokens)
	}
	if e.OutputTokens != 150 {
		t.Errorf("output = %d，想要 150；reasoning 已含在其中，加成 293 就是重复计费", e.OutputTokens)
	}
}

// 非用量事件不得产生流水，否则每回合会写进几十条垃圾行。
func TestParseUsageIgnoresOtherEvents(t *testing.T) {
	for _, line := range []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`,
		`{"type":"item/completed","item":{"type":"agentMessage"}}`,
		`{"type":"turn.failed","error":{"message":"boom"}}`,
		`{"type":"result","usage":{}}`, // 全零：没有实际消耗
		`not json at all`,
		``,
	} {
		if evs, _ := parseUsage([]byte(line), baseEvent()); len(evs) != 0 {
			t.Errorf("不该产生流水: %s -> %+v", line, evs)
		}
	}
}

// 起标题这一趟的消耗也要进流水，并且能和用户自己的对话区分开——两者都可能
// 落在 haiku 上，只看 model 分不出来。
func TestParseUsageTitleTurn(t *testing.T) {
	const titleJSON = `{"type":"result","subtype":"success","duration_ms":1842,` +
		`"result":"修复备份丢 WAL 的问题","total_cost_usd":0.000621,` +
		`"usage":{"input_tokens":531,"output_tokens":18},` +
		`"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":531,"outputTokens":18,` +
		`"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.000621}}}`

	base := baseEvent()
	base.Model = ""
	base.Kind = store.UsageKindTitle
	evs, _ := parseUsage([]byte(titleJSON), base)
	if len(evs) != 1 {
		t.Fatalf("应出 1 行，得到 %d 行", len(evs))
	}
	e := evs[0]
	if e.Kind != store.UsageKindTitle {
		t.Errorf("kind = %q, 想要 %q", e.Kind, store.UsageKindTitle)
	}
	if e.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q，应从 modelUsage 取到实际模型", e.Model)
	}
	if e.InputTokens != 531 || e.OutputTokens != 18 || e.CostMicroUSD != 621 {
		t.Errorf("标题回合用量不对: %+v", e)
	}
}

// 同一回合里 claude 补报的后续 result：子代理跑完一个补一条完成通知，撞上 429
// 每次重试也补一条报错的。形状取自线上那次重复扣款的现场（会话 2902935ecc8f）：
// 三条的 modelUsage 完全一致（就是本次运行的累计值），只有 total_cost_usd 在涨。
const claudeTaskNotifySample = `{"type":"result","subtype":"success","duration_ms":749,` +
	`"origin":{"kind":"task-notification"},"total_cost_usd":14.957196,` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":1044,"cache_read_input_tokens":68152,"output_tokens":72},` +
	`"modelUsage":{` +
	`"claude-haiku-4-5-20251001":{"inputTokens":531,"outputTokens":18,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.000621},` +
	`"claude-opus-4-8":{"inputTokens":2,"outputTokens":1870,"cacheReadInputTokens":7342,"cacheCreationInputTokens":5265,"costUSD":0.103081}}}`

const claudeRateLimitSample = `{"type":"result","subtype":"success","is_error":true,"duration_ms":517,` +
	`"api_error_status":429,"total_cost_usd":25.868015,` +
	`"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0},` +
	`"modelUsage":{` +
	`"claude-haiku-4-5-20251001":{"inputTokens":531,"outputTokens":18,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.000621},` +
	`"claude-opus-4-8":{"inputTokens":2,"outputTokens":1870,"cacheReadInputTokens":7342,"cacheCreationInputTokens":5265,"costUSD":0.103081}}}`

// 一个回合收到多个 claude result 时只能认最后一个：它们报的是本次运行的累计
// 值，相加就是把同一笔钱扣好几遍（线上真扣了 14 遍，$25.87 变成 $362）。
func TestUsageTallyClaudeResultsDoNotAccumulate(t *testing.T) {
	var tally usageTally
	tally.observe(baseEvent(), []byte(claudeResultSample))
	tally.observe(baseEvent(), []byte(claudeTaskNotifySample))
	tally.observe(baseEvent(), []byte(claudeRateLimitSample))

	if len(tally.evs) != 2 {
		t.Fatalf("三个 result 只该留下最后一个的两行，得到 %d 行", len(tally.evs))
	}
	var total int64
	for _, e := range tally.evs {
		total += e.CostMicroUSD
	}
	if want := int64(621 + 103081); total != want {
		t.Errorf("合计 %d 微美元，想要 %d——重复的 result 被算了不止一遍", total, want)
	}
	// 最后一条是 429 报错事件，顶层 usage 全 0，但 modelUsage 仍是累计值，
	// 记的必须是 modelUsage 那份，不能被清零。
	if tally.evs[1].OutputTokens != 1870 {
		t.Errorf("末条 result 的 token 数应取自 modelUsage: %+v", tally.evs[1])
	}
}

// 不认识的事件不该动汇总：一条真用量后面跟一串杂事件，账还是那份账。
func TestUsageTallyIgnoresNonUsageLines(t *testing.T) {
	var tally usageTally
	tally.observe(baseEvent(), []byte(claudeResultSample))
	tally.observe(baseEvent(), []byte(`{"type":"assistant","message":{"content":[]}}`))
	tally.observe(baseEvent(), []byte(`not json`))
	if len(tally.evs) != 2 {
		t.Fatalf("汇总应保持 2 行，得到 %d 行", len(tally.evs))
	}
}

// codex 的 turn.completed 报的是本回合自己的增量，多条就该累加——和 claude 的
// 累计式 result 相反，别把这两种语义混成一种。
func TestUsageTallyCodexTurnsAccumulate(t *testing.T) {
	base := baseEvent()
	base.Agent = "codex"
	var tally usageTally
	tally.observe(base, []byte(codexTurnSample))
	tally.observe(base, []byte(codexExecSample))
	if len(tally.evs) != 2 {
		t.Fatalf("两个 turn.completed 该各记一行，得到 %d 行", len(tally.evs))
	}
}
