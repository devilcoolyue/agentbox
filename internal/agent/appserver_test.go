package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
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
	c.translateItem(true, asItem{Type: "agentMessage", Text: "final", Phase: "final_answer", ID: "message-1"})
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
	Type  string `json:"type"`
	Usage struct {
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
