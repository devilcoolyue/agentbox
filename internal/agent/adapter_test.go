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
