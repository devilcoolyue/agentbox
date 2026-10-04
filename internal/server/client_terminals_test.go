package server

import (
	"strings"
	"testing"

	"agentbox/internal/store"
)

func TestClientTerminalCommandSeparatesSocketsAndQuotesArguments(t *testing.T) {
	sess := store.Session{Agent: "claude"}
	p := store.ClientProject{Path: "a'b"}
	tm := store.ClientTerminal{ID: "123456abcdef", Kind: "agent", Arguments: []string{"--model", "x'; touch /tmp/no; '"}}
	command, err := clientTerminalCommand(sess, p, tm, []string{"SYNTHETIC_SECRET=never-inline-this", "BAD;KEY=no"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tmux -L abox-client-123456abcdef", "new-session -d -s work", "/workspace/a", "set-environment -g SYNTHETIC_SECRET \"$SYNTHETIC_SECRET\""} {
		if !strings.Contains(command, want) {
			t.Fatalf("missing %q: %s", want, command)
		}
	}
	for _, bad := range []string{"never-inline-this", "BAD;KEY", "-s main", "--dangerously-bypass"} {
		if strings.Contains(command, bad) {
			t.Fatalf("unexpected %q", bad)
		}
	}
	tm.ID = "x; echo bad"
	if _, err = clientTerminalCommand(sess, p, tm, nil); err == nil {
		t.Fatal("unsafe persisted ID")
	}
	if !strings.HasSuffix(termCommand(nil), "exec tmux -u new-session -A -D -s main") {
		t.Fatal("legacy default terminal changed")
	}
}
