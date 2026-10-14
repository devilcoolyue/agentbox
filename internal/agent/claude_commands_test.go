package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// What Claude Code answers to initialize, trimmed to the parts we read plus
// noise the probe must skip.
const initializeOutput = `{"type":"system","subtype":"hook_started"}
{"type":"control_response","response":{"subtype":"success","request_id":"someone-else","response":{"commands":[{"name":"wrong"}]}}}
{"type":"control_response","response":{"subtype":"success","request_id":"agentbox-commands","response":{"models":[{"value":"default"}],"commands":[` +
	`{"name":"compact","description":"Free up context by\nsummarizing","argumentHint":"<optional custom summarization instructions>","builtin":true},` +
	`{"name":"clear","description":"Start a new session","argumentHint":"[name]","aliases":["reset","new","bad alias"],"builtin":true},` +
	`{"name":"anthropic-skills:pdf","description":"Use this skill whenever the user wants to do anything with PDF files."},` +
	`{"name":"compact","description":"duplicate"},` +
	`{"name":"../../etc","description":"not a command name"},` +
	`{"name":"","description":"empty"}]}}}
`

func TestProbeClaudeCommands(t *testing.T) {
	var sent bytes.Buffer
	got, err := ProbeClaudeCommands(&sent, strings.NewReader(initializeOutput))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"request":{"subtype":"initialize"},"request_id":"agentbox-commands","type":"control_request"}` + "\n"; sent.String() != want {
		t.Fatalf("sent %q", sent.String())
	}
	if len(got) != 3 || got[0].Name != "compact" || got[1].Name != "clear" || got[2].Name != "anthropic-skills:pdf" {
		t.Fatalf("%+v", got)
	}
	if got[0].Description != "Free up context by summarizing" || got[0].ArgumentHint == "" || !got[0].Builtin {
		t.Fatalf("%+v", got[0])
	}
	if strings.Join(got[1].Aliases, ",") != "reset,new" {
		t.Fatalf("aliases %v", got[1].Aliases)
	}
}

func TestProbeClaudeCommandsFailures(t *testing.T) {
	if _, err := ProbeClaudeCommands(io.Discard, strings.NewReader(`{"type":"control_response","response":{"subtype":"error","request_id":"agentbox-commands","error":"not now"}}`+"\n")); err == nil || !strings.Contains(err.Error(), "not now") {
		t.Fatal(err)
	}
	if _, err := ProbeClaudeCommands(io.Discard, strings.NewReader(`{"type":"result"}`+"\n")); err == nil {
		t.Fatal("missing response accepted")
	}
}

// Real CLI, no prompt: free, but needs Claude Code on PATH.
//
//	CLAUDE_COMMANDS_LIVE_TEST=1 go test -run TestProbeClaudeCommandsLive ./internal/agent/
func TestProbeClaudeCommandsLive(t *testing.T) {
	if os.Getenv("CLAUDE_COMMANDS_LIVE_TEST") != "1" {
		t.Skip("set CLAUDE_COMMANDS_LIVE_TEST=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	argv := ClaudeCommandsCommand()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = t.TempDir()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	got, err := ProbeClaudeCommands(stdin, stdout)
	stdin.Close()
	_ = cmd.Wait()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
	}
	if !names["compact"] || !names["context"] || len(got) < 10 {
		t.Fatalf("%d commands: %v", len(got), names)
	}
}
