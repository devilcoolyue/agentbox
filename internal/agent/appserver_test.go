package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
)

// 按真实 app-server 抓包整理的一回合协议脚本：resume 失败落到新开线程，
// 推理两段 summary、一次命令执行、正文两个 delta，最后正常收尾。
const turnScript = `{"id":1,"result":{"userAgent":"codex"}}
{"method":"configWarning","params":{"summary":"x"}}
{"id":2,"error":{"code":-32602,"message":"no rollout"}}
{"id":3,"result":{"thread":{"id":"T1"}}}
{"id":4,"result":{"turn":{"id":"U1","status":"inProgress"}}}
{"method":"item/started","params":{"item":{"type":"reasoning","id":"r1"}}}
{"method":"item/reasoning/summaryTextDelta","params":{"delta":"想"}}
{"method":"item/reasoning/summaryPartAdded","params":{}}
{"method":"item/reasoning/summaryTextDelta","params":{"delta":"再想"}}
{"method":"item/completed","params":{"item":{"type":"reasoning","id":"r1","summary":["想","再想"]}}}
{"method":"item/started","params":{"item":{"type":"commandExecution","command":"echo hi"}}}
{"method":"item/completed","params":{"item":{"type":"commandExecution","command":"echo hi","exitCode":0,"status":"completed"}}}
{"method":"item/started","params":{"item":{"type":"agentMessage","text":""}}}
{"method":"item/agentMessage/delta","params":{"delta":"你"}}
{"method":"item/agentMessage/delta","params":{"delta":"好"}}
{"method":"item/completed","params":{"item":{"type":"agentMessage","text":"你好"}}}
{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"last":{"inputTokens":10,"cachedInputTokens":4,"outputTokens":2}}}}
{"method":"turn/completed","params":{"turn":{"id":"U1","status":"completed"}}}
`

func TestRunCodexTurnTranslation(t *testing.T) {
	var wrote strings.Builder
	var events []string
	err := RunCodexTurn(context.Background(), &wrote, strings.NewReader(turnScript),
		func() {}, nil,
		CodexTurn{Prompt: "hi", ThreadID: "OLD", Model: "gpt-5.5", Effort: "low", Cwd: "/workspace"},
		func(line []byte) { events = append(events, string(line)) })
	if err != nil {
		t.Fatal(err)
	}

	// 发出的请求序列：initialize → initialized → resume（失败）→ start → turn/start
	var methods []string
	for _, line := range strings.Split(strings.TrimSpace(wrote.String()), "\n") {
		var m struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("非法请求行 %q: %v", line, err)
		}
		methods = append(methods, m.Method)
		if m.Method == "turn/start" {
			var p struct {
				ThreadID string `json:"threadId"`
				Model    string `json:"model"`
				Effort   string `json:"effort"`
				Summary  string `json:"summary"`
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			json.Unmarshal(m.Params, &p)
			if p.ThreadID != "T1" || p.Model != "gpt-5.5" || p.Effort != "low" || p.Summary != "auto" ||
				len(p.Input) != 1 || p.Input[0].Text != "hi" {
				t.Fatalf("turn/start 参数不对: %s", m.Params)
			}
		}
	}
	want := []string{"initialize", "initialized", "thread/resume", "thread/start", "turn/start"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("请求序列 %v, 期望 %v", methods, want)
	}

	// 翻译产物覆盖：会话 id、流式括号与增量、exec 形状的完整事件、回合收尾
	joined := "\n" + strings.Join(events, "\n") + "\n"
	for _, frag := range []string{
		`{"thread_id":"T1","type":"thread.started"}`,
		`"content_block":{"type":"thinking"}`,
		`"delta":{"thinking":"想","type":"thinking_delta"}`,
		`"delta":{"thinking":"\n\n","type":"thinking_delta"}`, // summary 分段的续接分隔
		`"item":{"text":"想\n\n再想","type":"reasoning"}`,
		`"item":{"command":"echo hi","type":"command_execution"}`,
		`"content_block":{"type":"text"}`,
		`"delta":{"text":"你","type":"text_delta"}`,
		`"item":{"text":"你好","type":"agent_message"}`,
		`"input_tokens":10`,
		`"type":"turn.completed"`,
	} {
		if !strings.Contains(joined, frag) {
			t.Errorf("事件流缺少 %s\n%s", frag, joined)
		}
	}

	// 每个流式块都成对闭合，且完整事件在块闭合之后
	if strings.Count(joined, "content_block_start") != 2 || strings.Count(joined, "content_block_stop") != 2 {
		t.Errorf("流式块括号不成对\n%s", joined)
	}
	if stop := strings.Index(joined, "content_block_stop"); stop < 0 ||
		strings.Index(joined, `"type":"reasoning"`) < stop {
		t.Errorf("完整事件先于块闭合\n%s", joined)
	}
}

func TestRunCodexTurnHandshakeFail(t *testing.T) {
	err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(""),
		func() {}, nil, CodexTurn{Prompt: "x"}, func([]byte) {})
	if !errors.Is(err, ErrAppServerUnavailable) {
		t.Fatalf("EOF 握手应报 ErrAppServerUnavailable, 得到 %v", err)
	}
}

func TestRunCodexTurnStrictResumeNeverStartsReplacement(t *testing.T) {
	for _, script := range []string{
		turnScript,
		`{"id":1,"result":{}}` + "\n" + `{"id":2,"result":{"thread":{"id":"different"}}}` + "\n",
	} {
		var sent strings.Builder
		err := RunCodexTurn(t.Context(), &sent, strings.NewReader(script), func() {}, nil, CodexTurn{ThreadID: "original", StrictResume: true}, func([]byte) {})
		if !errors.Is(err, ErrResumeUnavailable) || errors.Is(err, ErrAppServerUnavailable) {
			t.Fatal(err)
		}
		if strings.Contains(sent.String(), `"thread/start"`) || strings.Contains(sent.String(), `"turn/start"`) {
			t.Fatal("started replacement after failed resume")
		}
	}
}

func TestCodexTranslationPreservesFinalAnswerPhase(t *testing.T) {
	var output []byte
	c := &appServerConn{emit: func(raw []byte) { output = raw }}
	c.translateItem(true, rawItem{"type": json.RawMessage(`"agentMessage"`), "text": json.RawMessage(`"final"`), "phase": json.RawMessage(`"final_answer"`), "id": json.RawMessage(`"message-1"`)})
	if !strings.Contains(string(output), `"phase":"final_answer"`) || !strings.Contains(string(output), `"id":"message-1"`) {
		t.Fatal(string(output))
	}
}

func TestRunCodexTurnInterrupted(t *testing.T) {
	script := `{"id":1,"result":{}}
{"id":3,"result":{"thread":{"id":"T1"}}}
{"id":4,"result":{"turn":{"id":"U1"}}}
{"method":"turn/completed","params":{"turn":{"id":"U1","status":"interrupted"}}}
`
	var events []string
	err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(script),
		func() {}, nil, CodexTurn{Prompt: "x"},
		func(line []byte) { events = append(events, string(line)) })
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "回合已中断") {
		t.Fatalf("中断回合应产出 turn.failed 提示\n%s", joined)
	}
}

// 按 codex-cli 0.162.1 实测整理（合成 ID 与 token 数）：续聊进程在回合开始前
// 先补发上一回合的累计值（带旧 turnId），回合内每次模型请求一条通知，限流
// 更新会把同一份累计原样重发一遍。
const multiRequestScript = `{"id":1,"result":{"userAgent":"codex"}}
{"method":"thread/tokenUsage/updated","params":{"threadId":"T1","turnId":"OLD-TURN","tokenUsage":{"total":{"inputTokens":300,"cachedInputTokens":50,"outputTokens":17,"reasoningOutputTokens":2},"last":{"inputTokens":200,"cachedInputTokens":50,"outputTokens":9,"reasoningOutputTokens":1}}}}
{"id":2,"result":{"thread":{"id":"T1"}}}
{"id":4,"result":{"turn":{"id":"U2","status":"inProgress"}}}
{"method":"turn/started","params":{"threadId":"T1","turn":{"id":"U2"}}}
{"method":"thread/tokenUsage/updated","params":{"threadId":"T1","turnId":"U2","tokenUsage":{"total":{"inputTokens":600,"cachedInputTokens":150,"outputTokens":27,"reasoningOutputTokens":3},"last":{"inputTokens":300,"cachedInputTokens":100,"outputTokens":10,"reasoningOutputTokens":1}}}}
{"method":"thread/tokenUsage/updated","params":{"threadId":"T1","turnId":"U2","tokenUsage":{"total":{"inputTokens":600,"cachedInputTokens":150,"outputTokens":27,"reasoningOutputTokens":3},"last":{"inputTokens":300,"cachedInputTokens":100,"outputTokens":10,"reasoningOutputTokens":1}}}}
{"method":"thread/tokenUsage/updated","params":{"threadId":"T1","turnId":"U2","tokenUsage":{"total":{"inputTokens":1000,"cachedInputTokens":450,"outputTokens":39,"reasoningOutputTokens":4},"last":{"inputTokens":400,"cachedInputTokens":300,"outputTokens":12,"reasoningOutputTokens":1}}}}
{"method":"turn/completed","params":{"turn":{"id":"U2","status":"%s"}}}
`

type turnUsageEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Usage    struct {
		Input     int64 `json:"input_tokens"`
		Cached    int64 `json:"cached_input_tokens"`
		Output    int64 `json:"output_tokens"`
		Reasoning int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Requests []codexRequest `json:"requests"`
}

func runMultiRequest(t *testing.T, status string) turnUsageEvent {
	t.Helper()
	var last string
	err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(strings.Replace(multiRequestScript, "%s", status, 1)),
		func() {}, nil, CodexTurn{Prompt: "hi", ThreadID: "T1", Cwd: "/workspace"},
		func(line []byte) { last = string(line) })
	if err != nil {
		t.Fatal(err)
	}
	var ev turnUsageEvent
	if err := json.Unmarshal([]byte(last), &ev); err != nil {
		t.Fatalf("收尾事件 %q: %v", last, err)
	}
	return ev
}

func TestRunCodexTurnMetersEveryRequest(t *testing.T) {
	ev := runMultiRequest(t, "completed")
	if ev.Type != "turn.completed" {
		t.Fatalf("收尾事件类型 %q", ev.Type)
	}
	// 只算本回合的两次请求：恢复出来的旧累计只作基线，重发的那条去掉。
	if ev.Usage.Input != 700 || ev.Usage.Cached != 400 || ev.Usage.Output != 22 || ev.Usage.Reasoning != 2 {
		t.Fatalf("回合用量 %+v", ev.Usage)
	}
	if len(ev.Requests) != 2 {
		t.Fatalf("请求明细 %+v", ev.Requests)
	}
	first, second := ev.Requests[0], ev.Requests[1]
	if first.Input != 300 || first.Cached != 100 || first.Output != 10 ||
		second.Input != 400 || second.Cached != 300 || second.Output != 12 {
		t.Fatalf("请求明细 %+v", ev.Requests)
	}
	if first.TS == 0 || second.TS < first.TS {
		t.Fatalf("请求时间 %d / %d", first.TS, second.TS)
	}
	// 线程 id 跟着请求明细走：用量行靠它对上 Codex 遥测里的这一次请求。
	if ev.ThreadID == "" {
		t.Fatal("收尾事件缺线程 id")
	}
}

// 每认出一次本回合的请求就当场报一条单次用量（网页据此立即结算），与收尾
// 事件的 requests 逐项相同；它只给计量用，Decode 标为 Internal。
func TestRunCodexTurnReportsEachRequestAsItHappens(t *testing.T) {
	var events []string
	err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(strings.Replace(multiRequestScript, "%s", "completed", 1)),
		func() {}, nil, CodexTurn{Prompt: "hi", ThreadID: "T1", Cwd: "/workspace"},
		func(line []byte) { events = append(events, string(line)) })
	if err != nil {
		t.Fatal(err)
	}
	adapter, _ := Lookup(config.AgentCodex)
	var live []codexRequest
	for i, line := range events {
		var ev struct {
			Type     string       `json:"type"`
			ThreadID string       `json:"thread_id"`
			Request  codexRequest `json:"request"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != RequestUsageType {
			continue
		}
		if e, ok := adapter.Decode([]byte(line)); !ok || !e.Internal || e.Output || e.Partial || e.Terminal != "" {
			t.Fatalf("单次用量事件解码 %+v", e)
		}
		if ev.ThreadID != "T1" || i == len(events)-1 {
			t.Fatalf("单次用量事件 %s", line)
		}
		live = append(live, ev.Request)
	}
	var end turnUsageEvent
	if err := json.Unmarshal([]byte(events[len(events)-1]), &end); err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || !reflect.DeepEqual(live, end.Requests) {
		t.Fatalf("当场报的 %+v，收尾列的 %+v", live, end.Requests)
	}
}

// 远程压缩不进 tokenUsage（0.163 实测：压缩后累计不变、last 为 0）。收尾事件
// 带上回合 id 与压缩次数，结算时去会话记录里补那次请求；没压缩过的回合不带。
func TestRunCodexTurnReportsCompactions(t *testing.T) {
	script := strings.Replace(multiRequestScript, `{"method":"turn/completed"`,
		`{"method":"item/started","params":{"item":{"type":"contextCompaction","id":"c1"}}}
{"method":"thread/tokenUsage/updated","params":{"threadId":"T1","turnId":"U2","tokenUsage":{"total":{"inputTokens":1000,"cachedInputTokens":450,"outputTokens":39,"reasoningOutputTokens":4},"last":{"inputTokens":0,"cachedInputTokens":0,"outputTokens":0,"reasoningOutputTokens":0}}}}
{"method":"item/completed","params":{"item":{"type":"contextCompaction","id":"c1"}}}
{"method":"turn/completed"`, 1)
	var last string
	err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(strings.Replace(script, "%s", "completed", 1)),
		func() {}, nil, CodexTurn{Prompt: "hi", ThreadID: "T1", Cwd: "/workspace"},
		func(line []byte) { last = string(line) })
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		ThreadID    string         `json:"thread_id"`
		TurnID      string         `json:"turn_id"`
		Compactions int            `json:"compactions"`
		Requests    []codexRequest `json:"requests"`
	}
	if err := json.Unmarshal([]byte(last), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ThreadID != "T1" || ev.TurnID != "U2" || ev.Compactions != 1 || len(ev.Requests) != 2 {
		t.Fatalf("收尾事件 %s", last)
	}
	last = ""
	if err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(strings.Replace(multiRequestScript, "%s", "completed", 1)),
		func() {}, nil, CodexTurn{Prompt: "hi", ThreadID: "T1", Cwd: "/workspace"},
		func(line []byte) { last = string(line) }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(last, `"turn_id"`) || strings.Contains(last, `"compactions"`) || !strings.Contains(last, `"thread_id":"T1"`) {
		t.Fatalf("没压缩的回合: %s", last)
	}
}

// 中断的回合里已经发生的请求 provider 照样计费，收尾事件必须带上。
func TestRunCodexTurnInterruptedKeepsUsage(t *testing.T) {
	ev := runMultiRequest(t, "interrupted")
	if ev.Type != "turn.failed" || ev.Usage.Input != 700 || len(ev.Requests) != 2 {
		t.Fatalf("中断收尾 %+v", ev)
	}
}

// 压缩/重置后累计倒退：退回 last，且 last 不能超过新的累计。
func TestCodexMeterTotalReset(t *testing.T) {
	var m codexMeter
	at := time.Unix(1, 0)
	m.observe("U", "U", &codexTokens{Input: 500, Cached: 100, Output: 20}, &codexTokens{Input: 500, Cached: 100, Output: 20}, at)
	m.observe("U", "U", &codexTokens{Input: 80, Cached: 0, Output: 5}, &codexTokens{Input: 80, Output: 5}, at)
	m.observe("U", "U", &codexTokens{Input: 90, Output: 6}, &codexTokens{Input: 900, Output: 6}, at)
	got := []int64{}
	for _, r := range m.requests {
		got = append(got, r.Input, r.Output)
	}
	if fmt.Sprint(got) != "[500 20 80 5 10 1]" {
		t.Fatalf("请求 %v", got)
	}
}

// 协议形状按 codex-cli 0.162.1 的 generate-json-schema（ThreadTokenUsage 带
// modelContextWindow，TurnPlanUpdated 带完整计划）；ID 与数值为合成。
const planScript = `{"id":1,"result":{"userAgent":"codex"}}
{"id":2,"result":{"thread":{"id":"T1"}}}
{"id":4,"result":{"turn":{"id":"U3","status":"inProgress"}}}
{"method":"turn/plan/updated","params":{"threadId":"T1","turnId":"U3","explanation":"先读再改","plan":[{"step":"读配置","status":"completed"},{"step":"改代码","status":"inProgress"},{"step":"跑测试","status":"pending"}]}}
{"method":"thread/tokenUsage/updated","params":{"threadId":"T1","turnId":"U3","tokenUsage":{"total":{"inputTokens":900,"cachedInputTokens":100,"outputTokens":30,"reasoningOutputTokens":5,"totalTokens":930},"last":{"inputTokens":900,"cachedInputTokens":100,"outputTokens":30,"reasoningOutputTokens":5,"totalTokens":930},"modelContextWindow":272000}}}
{"method":"thread/tokenUsage/updated","params":{"threadId":"T1","turnId":"U3","tokenUsage":{"total":{"inputTokens":900,"cachedInputTokens":100,"outputTokens":30,"reasoningOutputTokens":5,"totalTokens":930},"last":{"inputTokens":900,"cachedInputTokens":100,"outputTokens":30,"reasoningOutputTokens":5,"totalTokens":930},"modelContextWindow":272000}}}
{"method":"turn/completed","params":{"turn":{"id":"U3","status":"completed"}}}
`

func TestRunCodexTurnForwardsPlanAndContext(t *testing.T) {
	var events []string
	err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(planScript), func() {}, nil,
		CodexTurn{Prompt: "hi", ThreadID: "T1", Cwd: "/workspace"}, func(line []byte) { events = append(events, string(line)) })
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	plan := `{"item":{"explanation":"先读再改","items":[{"completed":true,"status":"completed","text":"读配置"},{"completed":false,"status":"in_progress","text":"改代码"},{"completed":false,"status":"pending","text":"跑测试"}],"type":"todo_list"},"type":"item.updated"}`
	if !strings.Contains(joined, plan) {
		t.Fatalf("计划事件缺失\n%s", joined)
	}
	context := `{"info":{"last_token_usage":{"cached_input_tokens":100,"input_tokens":900,"output_tokens":30,"reasoning_output_tokens":5,"total_tokens":930},"model_context_window":272000},"type":"token_count"}`
	if strings.Count(joined, context) != 1 {
		t.Fatalf("上下文事件应恰好一条（重发的不再发）\n%s", joined)
	}
}

// 命令、改动与各类工具条目带上 CLI 显示所需的富字段；输出与 diff 有界落盘，
// 实时输出只走不落盘的 stream_event。
func TestRunCodexTurnRichItems(t *testing.T) {
	long := strings.Repeat("line\n", 100)
	out, _ := json.Marshal(long)
	diff := "--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n-old\n+new\n+more\n ctx\n"
	diffJSON, _ := json.Marshal(diff)
	script := `{"id":1,"result":{}}
{"id":3,"result":{"thread":{"id":"T1"}}}
{"id":4,"result":{"turn":{"id":"U1"}}}
{"method":"item/started","params":{"item":{"type":"commandExecution","id":"c1","command":"/bin/bash -lc 'sed -n 1,9p a.go'","cwd":"file:///workspace","status":"inProgress","commandActions":[{"type":"read","command":"sed -n 1,9p a.go","name":"a.go","path":"/workspace/a.go"}]}}}
{"method":"item/commandExecution/outputDelta","params":{"itemId":"c1","delta":"line\n"}}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"c1","command":"/bin/bash -lc 'sed -n 1,9p a.go'","status":"failed","exitCode":2,"durationMs":1234,"aggregatedOutput":` + string(out) + `,"commandActions":[{"type":"read","command":"sed -n 1,9p a.go","name":"a.go","path":"/workspace/a.go"}],"cwd":{"unexpected":"object"}}}}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"c2","command":"true","status":"completed","exitCode":0,"aggregatedOutput":""}}}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"c3","command":"true","status":"completed","exitCode":0,"aggregatedOutput":null}}}
{"method":"item/completed","params":{"item":{"type":"fileChange","id":"f1","status":"completed","changes":[{"path":"/workspace/x.go","kind":{"type":"update","move_path":null},"diff":` + string(diffJSON) + `}]}}}
{"method":"item/started","params":{"item":{"type":"mcpToolCall","id":"m1","server":"docs","tool":"search","status":"inProgress","arguments":{"q":"hi"}}}}
{"method":"item/mcpToolCall/progress","params":{"itemId":"m1","message":"half way"}}
{"method":"item/completed","params":{"item":{"type":"mcpToolCall","id":"m1","server":"docs","tool":"search","status":"failed","arguments":{"q":"hi"},"durationMs":20,"error":{"message":"boom"},"result":{"content":[{"type":"text","text":"partial"},{"type":"image","data":"…"}]}}}}
{"method":"item/completed","params":{"item":{"type":"webSearch","id":"w1","query":"codex cli","action":{"type":"search","queries":["codex cli","codex tui"]},"results":[{"title":"Codex","url":"https://example.com","domain":"example.com","snippet":"x"}]}}}
{"method":"item/completed","params":{"item":{"type":"imageView","id":"i1","path":"file:///shared/.images/a.png"}}}
{"method":"item/completed","params":{"item":{"type":"userMessage","id":"u1","content":[]}}}
{"method":"warning","params":{"message":"heads up"}}
{"method":"model/rerouted","params":{"fromModel":"a","toModel":"b","reason":"highRiskCyberActivity","threadId":"T1","turnId":"U1"}}
{"method":"error","params":{"error":{"message":"reconnecting"},"willRetry":true,"threadId":"T1","turnId":"U1"}}
{"method":"turn/completed","params":{"turn":{"id":"U1","status":"completed"}}}
`
	var events []map[string]any
	err := RunCodexTurn(context.Background(), io.Discard, strings.NewReader(script), func() {}, nil,
		CodexTurn{Prompt: "x", Cwd: "/workspace"}, func(line []byte) {
			var ev map[string]any
			if err := json.Unmarshal(line, &ev); err != nil {
				t.Fatalf("非法事件 %s", line)
			}
			events = append(events, ev)
		})
	if err != nil {
		t.Fatal(err)
	}
	find := func(typ, itemType string) map[string]any {
		for _, ev := range events {
			if ev["type"] != typ {
				continue
			}
			if itemType == "" {
				return ev
			}
			if it, _ := ev["item"].(map[string]any); it["type"] == itemType {
				return it
			}
		}
		t.Fatalf("缺少 %s/%s: %v", typ, itemType, events)
		return nil
	}

	started := find("item.started", "command_execution")
	if started["id"] != "c1" || started["cwd"] != "/workspace" || started["status"] != nil {
		t.Fatalf("started 命令: %v", started)
	}
	acts, _ := started["actions"].([]any)
	if len(acts) != 1 || acts[0].(map[string]any)["type"] != "read" || acts[0].(map[string]any)["name"] != "a.go" {
		t.Fatalf("命令归类: %v", started["actions"])
	}
	delta := find("stream_event", "")["event"].(map[string]any)
	if delta["type"] != "item_output_delta" || delta["item_id"] != "c1" || delta["delta"] != "line\n" {
		t.Fatalf("实时输出: %v", delta)
	}
	cmd := find("item.completed", "command_execution")
	// cwd 换了类型只丢这一个字段，条目照常落盘
	if cmd["status"] != "failed" || cmd["exit_code"] != 2.0 || cmd["duration_ms"] != 1234.0 || cmd["cwd"] != nil {
		t.Fatalf("完成命令: %v", cmd)
	}
	head, tail := cmd["aggregated_output"].(string), cmd["output_tail"].(string)
	if cmd["output_lines"] != 100.0 || cmd["output_truncated"] != true ||
		strings.Count(head, "line") != outputSideLines || strings.Count(tail, "line") != outputSideLines {
		t.Fatalf("输出预览: lines=%v head=%q tail=%q", cmd["output_lines"], head, tail)
	}

	for _, ev := range events {
		it, _ := ev["item"].(map[string]any)
		if ev["type"] != "item.completed" || it["type"] != "command_execution" {
			continue
		}
		switch _, has := it["output_lines"]; it["id"] {
		case "c2": // 确实没输出
			if it["output_lines"] != 0.0 || it["aggregated_output"] != nil {
				t.Fatalf("空输出: %v", it)
			}
		case "c3": // 没给输出，不能说成「无输出」
			if has {
				t.Fatalf("缺输出: %v", it)
			}
		}
	}

	fc := find("item.completed", "file_change")
	ch := fc["changes"].([]any)[0].(map[string]any)
	if ch["path"] != "/workspace/x.go" || ch["kind"] != "update" || ch["added"] != 2.0 || ch["removed"] != 1.0 || ch["diff"] != diff || ch["move_path"] != nil {
		t.Fatalf("改动: %v", ch)
	}

	if m := find("item.started", "mcp_tool_call"); m["server"] != "docs" || m["arguments"] != `{"q":"hi"}` {
		t.Fatalf("MCP started: %v", m)
	}
	mcp := find("item.completed", "mcp_tool_call")
	if mcp["status"] != "failed" || mcp["result_text"] != "partial\n[image]" || mcp["error"].(map[string]any)["message"] != "boom" {
		t.Fatalf("MCP: %v", mcp)
	}
	ws := find("item.completed", "web_search")
	if ws["query"] != "codex cli" || ws["result_count"] != 1.0 || ws["action"].(map[string]any)["type"] != "search" ||
		len(ws["results"].([]any)) != 1 || ws["results"].([]any)[0].(map[string]any)["snippet"] != nil {
		t.Fatalf("网页搜索: %v", ws)
	}
	if iv := find("item.completed", "image_view"); iv["path"] != "/shared/.images/a.png" {
		t.Fatalf("看图: %v", iv)
	}
	if w := find("warning", ""); w["message"] != "heads up" {
		t.Fatalf("警告: %v", w)
	}
	if r := find("model_rerouted", ""); r["to_model"] != "b" || r["reason"] != "high_risk_cyber_activity" {
		t.Fatalf("改道: %v", r)
	}
	if e := find("error", ""); e["will_retry"] != true {
		t.Fatalf("重试中的错误: %v", e)
	}
	for _, ev := range events {
		if it, _ := ev["item"].(map[string]any); it != nil && it["type"] == "userMessage" {
			t.Fatalf("用户消息回显不该落盘: %v", ev)
		}
	}
}

func TestCodexLiveOutputIsBounded(t *testing.T) {
	var sent int
	c := &appServerConn{emit: func(raw []byte) {
		var ev struct {
			Event struct {
				Delta string `json:"delta"`
			} `json:"event"`
		}
		json.Unmarshal(raw, &ev)
		sent += len(ev.Event.Delta)
	}}
	chunk, _ := json.Marshal(map[string]string{"itemId": "c1", "delta": strings.Repeat("日", 10000)})
	for range 5 {
		c.outputDelta(chunk)
	}
	if sent > liveOutputBytes || sent < liveOutputBytes-3 {
		t.Fatalf("实时输出转发了 %d 字节，上限 %d", sent, liveOutputBytes)
	}
}
