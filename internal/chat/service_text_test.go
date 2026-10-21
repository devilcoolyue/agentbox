package chat

import (
	"testing"

	"agentbox/internal/agent"
)

// The delegation reminder goes before ordinary prompts only: in front of a
// slash command the CLI would no longer see the command.
func TestCLITextKeepsSlashCommandFirst(t *testing.T) {
	in := Input{Preface: "<reminder>"}
	if got := cliText(in, "fix the bug"); got != "<reminder>\n\nfix the bug" {
		t.Fatalf("prompt: %q", got)
	}
	if got := cliText(in, "/context"); got != "/context" {
		t.Fatalf("command: %q", got)
	}
	if got := cliText(in, "/workspace/a.go 是什么"); got != "<reminder>\n\n/workspace/a.go 是什么" {
		t.Fatalf("path: %q", got)
	}
	if got := cliText(Input{Context: "ctx"}, "/context"); got != "/context\n\nctx" {
		t.Fatalf("context: %q", got)
	}
}

// Only a turn whose prefix a later request can repeat leaves a cache point.
func TestKeepableTurns(t *testing.T) {
	if !keepable("claude", Input{}, agent.ChatExtras{Interactive: true, TaskTools: true, PromptSuggestions: true}) {
		t.Fatal("ordinary Claude turn")
	}
	for name, c := range map[string]struct {
		agent  string
		in     Input
		extras agent.ChatExtras
	}{
		"codex":         {"codex", Input{}, agent.ChatExtras{}},
		"worker":        {"claude", Input{Fresh: true}, agent.ChatExtras{}},
		"delegate tool": {"claude", Input{}, agent.ChatExtras{MCPConfig: "/x.json"}},
		"system prompt": {"claude", Input{}, agent.ChatExtras{AppendSystemPrompt: "x"}},
		"allowed tools": {"claude", Input{}, agent.ChatExtras{AllowedTools: []string{"x"}}},
		"denied tools":  {"claude", Input{}, agent.ChatExtras{DisallowedTools: []string{"Agent"}}},
	} {
		if keepable(c.agent, c.in, c.extras) {
			t.Errorf("%s turn kept", name)
		}
	}
}
