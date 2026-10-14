package chat

import "testing"

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
