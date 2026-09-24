package credentials

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbox/internal/config"
	"github.com/pelletier/go-toml/v2"
)

func TestCodexOAuthSwitchPreservesSettingsAndAuthorizedHomes(t *testing.T) {
	acct := config.Account{ID: "codex", Type: config.AgentCodex, CredentialsDir: t.TempDir(), Access: &config.AccountAccess{Mode: "users", Users: []string{"alice"}}}
	svc, data := testService(t, "unused", acct)
	svc.sessions = testSessions{{ID: "s1", User: "alice", Agent: "codex", AccountID: acct.ID}, {ID: "s2", User: "bob", Agent: "codex", AccountID: acct.ID}}
	original := `model = "custom-model"
model_provider = "relay"
profile = "work"
model_reasoning_effort = "high"
forced_login_method = "api"
[model_providers.openai]
base_url = "https://old.invalid"
[model_providers.relay]
base_url = "https://relay.invalid"
[profiles.work]
model_provider = "relay"
model = "profile-model"
[mcp_servers.example]
command = "example"
`
	put(t, filepath.Join(acct.CredentialsDir, "config.toml"), original)
	for _, sess := range svc.sessions.All() {
		home := filepath.Join(data, "users", sess.User, "sessions", sess.ID, "home", ".codex")
		put(t, filepath.Join(home, "auth.json"), `{"OPENAI_API_KEY":"old"}`)
		put(t, filepath.Join(home, "config.toml"), original)
	}
	if err := svc.SaveCodexOAuth(t.Context(), acct.ID, CodexOAuthTokens{AccessToken: "access", RefreshToken: "refresh", IDToken: "id"}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{acct.CredentialsDir, filepath.Join(data, "users", "alice", "sessions", "s1", "home", ".codex")} {
		raw, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
		var got map[string]any
		if err := toml.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got["model_provider"] != "openai" || got["model"] != "custom-model" || got["model_reasoning_effort"] != "high" || got["forced_login_method"] != "chatgpt" || got["mcp_servers"] == nil {
			t.Fatalf("bad switch: %s", raw)
		}
		profile := got["profiles"].(map[string]any)["work"].(map[string]any)
		if profile["model_provider"] != "openai" || profile["model"] != "profile-model" {
			t.Fatal(profile)
		}
		if got["model_providers"].(map[string]any)["openai"] != nil {
			t.Fatal("official provider still overridden")
		}
		if CodexAuthMode(config.Account{CredentialsDir: dir}) != "oauth" {
			t.Fatal("OAuth credentials not published")
		}
	}
	bob := filepath.Join(data, "users", "bob", "sessions", "s2", "home", ".codex")
	if SavedAPIKey(bob) != "old" {
		t.Fatal("credentials delivered to unauthorized home")
	}
	if err := svc.SaveCodexKey(t.Context(), acct.ID, "new-key", "https://new.invalid/v1", "responses"); err != nil {
		t.Fatal(err)
	}
	if base, _ := ReadCodexProvider(acct.CredentialsDir); base != "https://new.invalid/v1" {
		t.Fatal("active profile masks relay")
	}
	if err := svc.SaveCodexKey(t.Context(), acct.ID, "official-key", "", "responses"); err != nil {
		t.Fatal(err)
	}
	if base, _ := ReadCodexProvider(acct.CredentialsDir); base != "" {
		t.Fatal("blank URL retained relay")
	}
	// A pre-switch CLI may rewrite its old OAuth auth.json after saving a Key.
	alice := filepath.Join(data, "users", "alice", "sessions", "s1", "home", ".codex")
	stale := filepath.Join(alice, "auth.json")
	put(t, stale, `{"tokens":{"access_token":"old-access","refresh_token":"old-refresh"}}`)
	future := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(stale, future, future); err != nil {
		t.Fatal(err)
	}
	if err := svc.Sync(t.Context(), acct, svc.sessions.All()[0]); err != nil {
		t.Fatal(err)
	}
	if SavedAPIKey(acct.CredentialsDir) != "official-key" || SavedAPIKey(alice) != "official-key" {
		t.Fatal("stale CLI restored previous authentication mode")
	}
}

func TestCodexStatusParsesCredentials(t *testing.T) {
	acct := config.Account{Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	for _, tt := range []struct{ raw, status string }{{`{"OPENAI_API_KEY":null}`, "missing"}, {`{"OPENAI_API_KEY":""}`, "missing"}, {`{"OPENAI_API_KEY":"key"}`, "ok"}, {`{"tokens":{"access_token":"a","refresh_token":"r"}}`, "ok"}, {`{"tokens":{"access_token":"a"}}`, "norefresh"}, {`{"text":"OPENAI_API_KEY"}`, "missing"}} {
		put(t, filepath.Join(acct.CredentialsDir, "auth.json"), tt.raw)
		if status, _ := Status(acct); status != tt.status {
			t.Errorf("%s: %s", tt.raw, status)
		}
	}
}
