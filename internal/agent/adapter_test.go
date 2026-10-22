package agent

import (
	"os"
	"strings"
	"testing"
)

func TestAdapterProtocolFixtures(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		a, err := Lookup(kind)
		if err != nil {
			t.Fatal(err)
		}
		if a.Capabilities().AppServer != (kind == "codex") || !a.Capabilities().TerminalUsage {
			t.Fatal("capabilities")
		}
		raw, err := os.ReadFile("testdata/" + kind + "-stream.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		for i, line := range lines {
			e, ok := a.Decode([]byte(line))
			if !ok {
				t.Fatal("fixture decode")
			}
			if i == 0 && (e.Output || e.SessionID != "fixture-"+kind) {
				t.Fatalf("init/resume: %+v", e)
			}
			if i == 1 && (!e.Partial || !e.Output) {
				t.Fatal("partial event lost")
			}
			if i == 2 && (e.Partial || !e.Output) {
				t.Fatal("complete output lost")
			}
		}
		if _, ok := a.Decode([]byte("null")); ok {
			t.Fatal("null decoded as an event")
		}
		if _, ok := a.Decode([]byte("not json")); ok {
			t.Fatal("accepted raw text as protocol")
		}
		if _, err := a.Chat("bypassPermissions", "fixture-id", "fixture-model", "high"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Lookup("future-agent"); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestAdapterTerminalOutcomesAreExplicit(t *testing.T) {
	for _, tc := range []struct{ kind, raw, want string }{
		{"claude", `{"type":"result","subtype":"success","is_error":false}`, "completed"},
		{"claude", `{"type":"result","subtype":"success","is_error":true}`, "failed"},
		{"claude", `{"type":"result","subtype":"error_max_turns"}`, "failed"},
		{"claude", `{"type":"result","subtype":"future_status"}`, ""},
		{"claude", `{"type":"assistant"}`, ""},
		{"codex", `{"type":"turn.completed","usage":{"input_tokens":1}}`, "completed"},
		{"codex", `{"type":"turn.failed","status":"interrupted"}`, "interrupted"},
		{"codex", `{"type":"turn.failed","error":{"message":"回合已中断"}}`, "failed"},
		{"codex", `{"type":"thread.started","thread_id":"x"}`, ""},
	} {
		a, _ := Lookup(tc.kind)
		e, ok := a.Decode([]byte(tc.raw))
		if !ok || e.Terminal != tc.want {
			t.Fatalf("%s %s: %+v", tc.kind, tc.raw, e)
		}
	}
}

// Claude's thinking token counts tick every few tokens while the model thinks;
// like content deltas they are broadcast but never persisted.
func TestAdapterThinkingProgressIsPartial(t *testing.T) {
	a, _ := Lookup("claude")
	for _, tc := range []struct {
		raw     string
		partial bool
	}{
		{`{"type":"system","subtype":"thinking_tokens","estimated_tokens":5,"estimated_tokens_delta":4}`, true},
		{`{"type":"system","subtype":"status","status":"requesting"}`, false},
		{`{"type":"system","subtype":"task_started","task_id":"x"}`, false},
		{`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"..."}]}}`, false},
	} {
		if e, ok := a.Decode([]byte(tc.raw)); !ok || e.Partial != tc.partial {
			t.Fatalf("%s: %+v", tc.raw, e)
		}
	}
}

// A result without any model round-trip is a local slash command: /context
// and friends answer from the CLI itself and have no usage to meter.
func TestAdapterMarksLocalClaudeResults(t *testing.T) {
	a, _ := Lookup("claude")
	for _, tc := range []struct {
		raw   string
		local bool
	}{
		{`{"type":"result","subtype":"success","is_error":false,"num_turns":0,"result":"## Context Usage"}`, true},
		{`{"type":"result","subtype":"success","is_error":false,"num_turns":3}`, false},
		{`{"type":"result","subtype":"success","is_error":false}`, false},
		{`{"type":"result","subtype":"success","is_error":true,"num_turns":0}`, false},
		{`{"type":"result","subtype":"error_during_execution","num_turns":0}`, false},
	} {
		if e, ok := a.Decode([]byte(tc.raw)); !ok || e.Local != tc.local {
			t.Fatalf("%s: %+v", tc.raw, e)
		}
	}
}

// The same rule as the CLI: a name right after the slash, up to whitespace.
func TestIsClaudeSlashCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"/context":                      true,
		"  /compact keep the API notes": true,
		"/code-review\nfocus on auth":   true,
		"/plugin:skill args":            true,
		"/_private":                     true,
		"/workspace/a.go 是什么":           false,
		"/":                             false,
		"/ context":                     false,
		"/-x":                           false,
		"context":                       false,
		"看下 /context":                   false,
		"/上下文":                          false,
	} {
		if got := IsClaudeSlashCommand(text); got != want {
			t.Errorf("%q: got %v", text, got)
		}
	}
}
