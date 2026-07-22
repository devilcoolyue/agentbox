package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
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
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			json.Unmarshal(m.Params, &p)
			if p.ThreadID != "T1" || p.Model != "gpt-5.5" || p.Effort != "low" ||
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
