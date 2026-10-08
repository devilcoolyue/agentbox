package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"agentbox/internal/config"
)

func writeHome(t *testing.T, home, rel, raw string) {
	t.Helper()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClearCredentialsClaudeKeepsEverythingButLogin(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeHome(t, home, ".claude/.credentials.json", `{"claudeAiOauth":{"refreshToken":"previous-test-refresh"}}`)
	writeHome(t, home, ".claude/settings.json", `{"model":"claude-opus-5"}`)
	writeHome(t, home, ".claude/projects/-workspace/thread.jsonl", "{}\n")
	writeHome(t, home, ".claude.json", `{"hasCompletedOnboarding":true,"firstStartTime":9007199254740993,"tip":"<a&b>","oauthAccount":{"emailAddress":"previous@example.test"}}`)

	if err := ClearCredentials(config.AgentClaude, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/.credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("login file survived: %v", err)
	}
	for _, rel := range []string{".claude/settings.json", ".claude/projects/-workspace/thread.jsonl"} {
		if _, err := os.Stat(filepath.Join(home, rel)); err != nil {
			t.Fatalf("%s removed: %v", rel, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "oauthAccount") || strings.Contains(string(raw), "previous@example.test") {
		t.Fatalf("cached account survived: %s", raw)
	}
	// Integers beyond float64 precision and HTML characters round-trip verbatim.
	for _, want := range []string{`"firstStartTime": 9007199254740993`, `"tip": "<a&b>"`, `"hasCompletedOnboarding": true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("state file lost %s: %s", want, raw)
		}
	}
}

func TestClearCredentialsCodexDropsConnectionOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeHome(t, home, ".codex/auth.json", `{"OPENAI_API_KEY":"previous-test-key"}`)
	writeHome(t, home, ".codex/config.toml", `model = "gpt-5.5"
model_provider = "agentbox"
forced_login_method = "api"
cli_auth_credentials_store = "file"
profile = "work"

[model_providers.agentbox]
base_url = "https://relay.example.test/v1"

[model_providers.mine]
base_url = "https://mine.example.test/v1"

[mcp_servers.docs]
command = "docs-mcp"

[profiles.work]
model = "gpt-5.5"
model_provider = "agentbox"
`)
	if err := ClearCredentials(config.AgentCodex, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex/auth.json")); !os.IsNotExist(err) {
		t.Fatalf("login file survived: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := toml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model_provider", "forced_login_method", "cli_auth_credentials_store"} {
		if _, ok := got[key]; ok {
			t.Fatalf("connection key %s survived: %s", key, raw)
		}
	}
	providers := got["model_providers"].(map[string]any)
	if _, ok := providers["agentbox"]; ok {
		t.Fatalf("relay provider survived: %s", raw)
	}
	if _, ok := providers["mine"]; !ok {
		t.Fatalf("user provider removed: %s", raw)
	}
	if got["model"] != "gpt-5.5" || got["mcp_servers"] == nil {
		t.Fatalf("model or MCP settings lost: %s", raw)
	}
	profile := got["profiles"].(map[string]any)["work"].(map[string]any)
	if _, ok := profile["model_provider"]; ok || profile["model"] != "gpt-5.5" {
		t.Fatalf("active profile not cleaned: %s", raw)
	}
}

func TestClearCredentialsToleratesFreshOrForeignState(t *testing.T) {
	// Never prepared, or never started: nothing to clear.
	if err := ClearCredentials(config.AgentClaude, filepath.Join(t.TempDir(), "missing", "home"), os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	writeHome(t, home, ".claude.json", `not json`)
	if err := ClearCredentials(config.AgentClaude, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, ".claude.json")); string(raw) != "not json" {
		t.Fatalf("rewrote a state file it could not parse: %q", raw)
	}
	writeHome(t, home, ".claude.json", `{"hasCompletedOnboarding":true}`)
	before, _ := os.Stat(filepath.Join(home, ".claude.json"))
	if err := ClearCredentials(config.AgentClaude, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(home, ".claude.json"))
	if !os.SameFile(before, after) {
		t.Fatal("rewrote a state file without a cached account")
	}
	var state map[string]any
	raw, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if json.Unmarshal(raw, &state) != nil || state["hasCompletedOnboarding"] != true {
		t.Fatalf("state changed: %s", raw)
	}
	if err := ClearCredentials("other", home, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("accepted an unknown agent")
	}
}
