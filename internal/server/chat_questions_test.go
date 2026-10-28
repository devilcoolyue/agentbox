package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

const questionHelp = "  --input-format <format>\n  --permission-prompt-tool <tool>\n  --replay-user-messages\n"

const askInput = `{"questions":[{"question":"Deploy where?","header":"Env","options":[{"label":"staging","description":"pre-release"},{"label":"production"}],"multiSelect":false}]}`

var askLines = []string{
	`{"type":"system","subtype":"init","session_id":"provider-session"}`,
	`{"type":"assistant","session_id":"provider-session","message":{"id":"msg-1","model":"claude-opus-5","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"AskUserQuestion","input":` + askInput + `}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
	`{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","display_name":"AskUserQuestion","input":` + askInput + `,"tool_use_id":"toolu_1","requires_user_interaction":true}}`,
}

var finishLines = []string{
	`{"type":"assistant","session_id":"provider-session","message":{"id":"msg-2","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":4,"output_tokens":1}}}`,
	`{"type":"result","subtype":"success","session_id":"provider-session"}`,
}

// fakeTurn is the CLI side of an interactive turn in the fake Docker engine.
type fakeTurn struct {
	t   *testing.T
	in  *bufio.Reader
	out io.Writer
}

func newFakeTurn(t *testing.T, in io.Reader, out io.Writer) fakeTurn {
	return fakeTurn{t, bufio.NewReader(in), out}
}

func (c fakeTurn) read() map[string]any {
	line, err := c.in.ReadBytes('\n')
	if err != nil {
		c.t.Errorf("stdin ended early: %v", err)
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		c.t.Errorf("stdin line %q", line)
	}
	return m
}

func (c fakeTurn) emit(lines ...string) {
	for _, line := range lines {
		_, _ = io.WriteString(c.out, line+"\n")
	}
}

// eof reports that stdin was closed with nothing more written to it.
func (c fakeTurn) eof() bool {
	rest, err := io.ReadAll(c.in)
	return err == nil && len(rest) == 0
}

func (c fakeTurn) prompt(want string) {
	m := c.read()
	msg, _ := m["message"].(map[string]any)
	if m["type"] != "user" || msg["role"] != "user" || msg["content"] != want {
		c.t.Errorf("prompt %v", m)
	}
}

// response digs the reply out of a control_response the host wrote.
func response(m map[string]any) (subtype string, body map[string]any) {
	outer, _ := m["response"].(map[string]any)
	body, _ = outer["response"].(map[string]any)
	subtype, _ = outer["subtype"].(string)
	if m["type"] != "control_response" {
		return "", nil
	}
	return subtype, body
}

func waitQuestion(t *testing.T, room *chatRoom, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		room.mu.Lock()
		pending := room.questions[id] != nil
		room.mu.Unlock()
		if pending {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("question never became answerable")
}

func (f *receiptFixture) questionHistory(t *testing.T) (views []chat.QuestionView, raw string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.s.chatsDir(f.sess), f.input.ThreadID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e logEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Kind != "question" {
			continue
		}
		var v chat.QuestionView
		if err := json.Unmarshal(e.Event, &v); err != nil {
			t.Fatal(err)
		}
		views = append(views, v)
	}
	return views, string(data)
}

func questionFixture(t *testing.T) *receiptFixture {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	f.help, f.input.Questions = questionHelp, true
	f.unblock()
	return f
}

// The model's question waits in the room for the first answer from any page;
// the CLI gets it back in the same turn, and only questions reach the user.
func TestChatQuestionAnswerContinuesTurn(t *testing.T) {
	f := questionFixture(t)
	replies := make(chan map[string]any, 3)
	closed := make(chan bool, 1)
	f.cli = func(in io.Reader, out io.Writer, _ string) {
		c := newFakeTurn(t, in, out)
		c.prompt(f.input.Text)
		c.emit(`{"type":"control_request","request_id":"req-bash","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"toolu_0"}}`,
			`{"type":"control_request","request_id":"req-hook","request":{"subtype":"hook_callback","callback_id":"x"}}`)
		replies <- c.read()
		replies <- c.read()
		c.emit(askLines...)
		replies <- c.read()
		c.emit(finishLines...)
		closed <- c.eof()
	}
	id := newOperationID()
	if c := receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input)); c.Request == nil || !c.Request.Questions {
		t.Fatal("questions not frozen with the request", c.Request)
	}
	room := f.s.chat.room(f.sess.ID)
	waitQuestion(t, room, "toolu_1")
	for _, bad := range []any{
		map[string]any{"answers": map[string]string{"Somewhere else?": "x"}},
		map[string]any{"answers": map[string]string{"Deploy where?": "  "}},
		map[string]any{"skip": true, "answers": map[string]string{"Deploy where?": "staging"}},
		map[string]any{},
	} {
		if w := f.request("POST", "questions/toolu_1", "alice", bad); w.Code != 400 {
			t.Fatal("accepted", bad, w.Code, w.Body.String())
		}
	}
	answer := map[string]any{"answers": map[string]string{"Deploy where?": "staging"}}
	if w := f.request("POST", "questions/toolu_1", "bob", answer); w.Code != 404 {
		t.Fatal("another user answered", w.Code)
	}
	if w := f.request("POST", "questions/toolu_1", "alice", answer); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := f.request("POST", "questions/toolu_1", "alice", answer); w.Code != 409 || !strings.Contains(w.Body.String(), "question_closed") {
		t.Fatal("second answer accepted", w.Code, w.Body.String())
	}
	if c := f.wait(t, id); c.State != store.ChatCompleted {
		t.Fatal(c)
	}
	if kind, body := response(<-replies); kind != "success" || body["behavior"] != "deny" {
		t.Fatal("other permission prompt not refused", kind, body)
	}
	if kind, _ := response(<-replies); kind != "error" {
		t.Fatal("unknown control request not answered", kind)
	}
	kind, body := response(<-replies)
	input, _ := body["updatedInput"].(map[string]any)
	answers, _ := input["answers"].(map[string]any)
	if kind != "success" || body["behavior"] != "allow" || answers["Deploy where?"] != "staging" || input["questions"] == nil {
		t.Fatal("answer not passed back", body)
	}
	if !<-closed {
		t.Fatal("stdin stayed open after the result")
	}
	var cmd string
	f.commands.Range(func(_, v any) bool {
		if s := v.(string); strings.Contains(s, "claude -p") {
			cmd = s
		}
		return true
	})
	if !strings.Contains(cmd, "--input-format stream-json --permission-prompt-tool stdio") {
		t.Fatal("command", cmd)
	}
	views, raw := f.questionHistory(t)
	if len(views) != 2 || views[0].State != "pending" || len(views[0].Questions) != 1 || views[0].Questions[0].Options[0].Description != "pre-release" ||
		views[0].ExpiresAt == nil || views[1].State != "answered" || views[1].Answers["Deploy where?"] != "staging" {
		t.Fatal("question history", views)
	}
	if strings.Contains(raw, "control_request") {
		t.Fatal("control messages leaked into history")
	}
}

// Without an answer the model is told to go on by itself, and the turn's own
// time limit does not count the wait.
func TestChatQuestionSkipAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, state, message string
	}{
		{"skip", "skipped", "skipped this question"},
		{"expire", "expired", "time limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "expire" {
				old := chat.QuestionTimeout
				chat.QuestionTimeout = 50 * time.Millisecond
				t.Cleanup(func() { chat.QuestionTimeout = old })
			}
			f := questionFixture(t)
			replies := make(chan map[string]any, 1)
			f.cli = func(in io.Reader, out io.Writer, _ string) {
				c := newFakeTurn(t, in, out)
				c.prompt(f.input.Text)
				c.emit(askLines...)
				replies <- c.read()
				c.emit(finishLines...)
				c.eof()
			}
			id := newOperationID()
			receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
			if tc.name == "skip" {
				waitQuestion(t, f.s.chat.room(f.sess.ID), "toolu_1")
				if w := f.request("POST", "questions/toolu_1", "alice", map[string]any{"skip": true}); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if c := f.wait(t, id); c.State != store.ChatCompleted {
				t.Fatal(c)
			}
			kind, body := response(<-replies)
			if msg, _ := body["message"].(string); kind != "success" || body["behavior"] != "deny" || !strings.Contains(msg, tc.message) {
				t.Fatal("reply", body)
			}
			if views, _ := f.questionHistory(t); len(views) != 2 || views[1].State != tc.state || views[1].Answers != nil {
				t.Fatal(views)
			}
		})
	}
}

// Interrupting while a question waits goes through the protocol: the CLI
// withdraws the question and the receipt ends interrupted, without SIGINT.
func TestChatQuestionInterruptWithdrawsQuestion(t *testing.T) {
	f := questionFixture(t)
	got := make(chan map[string]any, 1)
	closed := make(chan bool, 1)
	f.cli = func(in io.Reader, out io.Writer, _ string) {
		f.exitCode.Store(1) // after the --help probe, which shares the fixture's exit code
		c := newFakeTurn(t, in, out)
		c.prompt(f.input.Text)
		c.emit(askLines...)
		got <- c.read()
		c.emit(`{"type":"control_cancel_request","request_id":"req-ask"}`,
			`{"type":"control_response","response":{"subtype":"success","request_id":"agentbox-interrupt","response":{}}}`,
			`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"provider-session"}`)
		closed <- c.eof()
	}
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	room := f.s.chat.room(f.sess.ID)
	waitQuestion(t, room, "toolu_1")
	if err := room.activeTurn(t).interruptRequest(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if c := f.wait(t, id); c.State != store.ChatInterrupted {
		t.Fatal(c)
	}
	if m := <-got; m["type"] != "control_request" || m["request"].(map[string]any)["subtype"] != "interrupt" {
		t.Fatal("interrupt not sent in protocol", m)
	}
	if !<-closed || f.interruptions.Load() != 0 {
		t.Fatal("answer written after withdrawal or SIGINT used", f.interruptions.Load())
	}
	if views, _ := f.questionHistory(t); len(views) != 2 || views[1].State != "cancelled" {
		t.Fatal(views)
	}
	if w := f.request("POST", "questions/toolu_1", "alice", map[string]any{"skip": true}); w.Code != 409 {
		t.Fatal("withdrawn question still answerable", w.Code)
	}
	// The stop reads as an interruption, not as the CLI's failed result.
	var result, status []string
	entries, _ := f.historyEntries(t)
	for _, e := range entries {
		if e.Kind == "event" && strings.Contains(string(e.Event), `"type":"result"`) {
			result = append(result, e.State)
		}
		if e.Kind == "status" {
			status = append(status, e.State)
		}
	}
	if strings.Join(result, ",") != "interrupted" || strings.Join(status, ",") != "interrupted" {
		t.Fatal("history", result, status)
	}
}

// Pages that cannot show questions, and CLIs without the protocol, keep the
// one-shot turn the model cannot ask in.
func TestChatQuestionsFollowRequestAndCLI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request bool
		help    string
	}{
		{"older-cli", true, "  --effort <level>"},
		{"not-requested", false, questionHelp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentReceiptFixture(t, config.AgentClaude, claudeSuggestionOutput, false)
			f.help, f.input.Questions = tc.help, tc.request
			f.cli = func(io.Reader, io.Writer, string) { t.Error("interactive turn started") }
			f.unblock()
			id := newOperationID()
			receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
			if c := f.wait(t, id); c.State != store.ChatCompleted {
				t.Fatal(c)
			}
			f.commands.Range(func(_, v any) bool {
				if cmd := v.(string); strings.Contains(cmd, "claude -p") && strings.Contains(cmd, "--input-format") {
					t.Errorf("command %q", cmd)
				}
				return true
			})
		})
	}
}
