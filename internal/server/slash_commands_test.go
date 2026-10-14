package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

func (f *receiptFixture) slashCommands(t *testing.T, user string) (int, slashCommandsView) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/sessions/"+f.sess.ID+"/commands", nil)
	r.Header.Set("Authorization", "Bearer "+user)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	var view slashCommandsView
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(err, w.Body.String())
		}
	}
	return w.Code, view
}

// The composer's suggestions come from the container CLI's initialize answer:
// no prompt is written, and a minute's cache spares a CLI per keystroke.
func TestSlashCommandsFromContainerCLI(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	f.help = "  --input-format <format>\n  --no-session-persistence\n  --strict-mcp-config\n"
	f.cli = func(in io.Reader, out io.Writer, cmd string) {
		if !strings.Contains(cmd, strings.Join(agent.ClaudeCommandsCommand(), " ")) {
			t.Errorf("command %q", cmd)
		}
		line, _ := bufio.NewReader(in).ReadString('\n')
		if !strings.Contains(line, `"subtype":"initialize"`) || strings.Contains(line, `"type":"user"`) {
			t.Errorf("stdin %q", line)
		}
		_, _ = io.WriteString(out, `{"type":"control_response","response":{"subtype":"success","request_id":"agentbox-commands","response":{"commands":[{"name":"context","description":"Show current context usage","argumentHint":"","builtin":true},{"name":"review-pr","description":"Project command"}]}}}`+"\n")
	}
	for range 2 {
		code, view := f.slashCommands(t, "alice")
		if code != 200 || view.State != "available" || len(view.Commands) != 2 || view.Commands[0].Name != "context" || view.Commands[1].Name != "review-pr" {
			t.Fatal(code, view)
		}
	}
	if n := f.invocations.Load(); n != 1 {
		t.Fatalf("CLI started %d times", n)
	}
	if code, _ := f.slashCommands(t, "bob"); code == 200 {
		t.Fatal("another user's workspace listed")
	}
}

func TestSlashCommandsWithoutCLIOrContainer(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	f.help = "  --input-format <format>\n" // an image without the listing flags
	f.cli = func(io.Reader, io.Writer, string) { t.Error("CLI started without the needed flags") }
	if code, view := f.slashCommands(t, "alice"); code != 200 || view.State != "unavailable" || view.Commands == nil || len(view.Commands) != 0 {
		t.Fatal(code, view)
	}
	f.sess.Status = store.StatusStopped
	if err := f.s.store.Put(f.sess); err != nil {
		t.Fatal(err)
	}
	if _, view := f.slashCommands(t, "alice"); view.State != "stopped" {
		t.Fatal(view)
	}
	codex := newAgentReceiptFixture(t, config.AgentCodex, "", false)
	if _, view := codex.slashCommands(t, "alice"); view.State != "unavailable" {
		t.Fatal(view)
	}
}
