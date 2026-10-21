package agent

import (
	"strings"
	"testing"
)

func TestKeepaliveCommand(t *testing.T) {
	cmd, err := KeepaliveCommand("C1", "", "claude-opus-5", "high", "effort", true)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(cmd, " ")
	for _, want := range []string{"--resume C1", "--model claude-opus-5", "--effort high", "--permission-mode default",
		"--thinking-display summarized", "--output-format stream-json --include-partial-messages"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q: %q", want, cmd)
		}
	}
	if !strings.HasSuffix(line, "--max-turns 1 --no-session-persistence") {
		t.Fatalf("keepalive may persist or keep going: %q", cmd)
	}
	// Its own PID file: the user's stop must not reach it.
	if !strings.Contains(cmd[2], "echo $$ >"+KeepalivePidFile+";") || strings.Contains(cmd[2], PidFile) {
		t.Fatalf("pid file: %q", cmd[2])
	}
	if !strings.Contains(cmd[2], "export CLAUDE_CODE_ENABLE_TODO_TOOLS=1; ") {
		t.Fatalf("task tools change the cached tool list and must follow the turn: %q", cmd[2])
	}
	// A resumed session must not attach its changed instruction files again.
	if !strings.Contains(cmd[2], "export CLAUDE_CODE_DISABLE_CLAUDE_MDS='1'; ") {
		t.Fatalf("instruction files would be attached again: %q", cmd[2])
	}
	for _, unwanted := range []string{"--prompt-suggestions", "--input-format", "bypassPermissions"} {
		if strings.Contains(line, unwanted) {
			t.Fatalf("unexpected %q: %q", unwanted, cmd)
		}
	}
	// The legacy thinking budget is part of the request as well.
	cmd, err = KeepaliveCommand("C1", "", "claude-opus-4-5", "high", "", false)
	if err != nil || !strings.Contains(cmd[2], "MAX_THINKING_TOKENS=24000") || strings.Contains(cmd[2], "TODO_TOOLS") {
		t.Fatalf("budget: %v %q", err, cmd)
	}
	if _, err := KeepaliveCommand("", "", "claude-opus-5", "", "", false); err == nil {
		t.Fatal("keepalive without a conversation would start a new one")
	}
	if strings.Contains(cmd[2], "cd ") {
		t.Fatalf("a chat conversation runs in /workspace: %q", cmd[2])
	}
	// A terminal session resumes from the directory it ran in.
	cmd, err = KeepaliveCommand("T1", "/workspace/it's here", "claude-opus-5", "", "", false)
	if err != nil || !strings.Contains(cmd[2], `cd '/workspace/it'"'"'s here' || exit 1; `) || !strings.HasSuffix(cmd[2], `exec "$@"`) {
		t.Fatalf("dir: %v %q", err, cmd)
	}
	for _, dir := range []string{"relative", "/tmp/a\nb"} {
		if _, err := KeepaliveCommand("T1", dir, "claude-opus-5", "", "", false); err == nil {
			t.Fatalf("accepted dir %q", dir)
		}
	}
}
