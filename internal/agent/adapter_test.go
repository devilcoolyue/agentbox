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
