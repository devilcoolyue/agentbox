package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"net/http"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/pelletier/go-toml/v2"
)

const gatewayURL = "http://172.17.0.1:1083/x/s1/mac"

func codexGatewayService(t *testing.T, acct config.Account, endpoint func(config.Account, store.Session) string) (*Service, string) {
	t.Helper()
	data := t.TempDir()
	cfg := &config.Config{DataDir: data, Accounts: []config.Account{acct}}
	svc := New(cfg, testSessions{{ID: "s1", User: "alice", Agent: config.AgentCodex, AccountID: acct.ID}},
		func(_ config.Account, d time.Duration) (*http.Client, error) { return &http.Client{Timeout: d}, nil }, "test-public-client", func() string { return "" })
	svc.SetCodexEndpoint(endpoint)
	return svc, data
}

func codexHome(data string) string {
	return filepath.Join(data, "users", "alice", "sessions", "s1", "home", ".codex")
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := toml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func providerBase(settings map[string]any, name string) string {
	providers, _ := settings["model_providers"].(map[string]any)
	p, _ := providers[name].(map[string]any)
	base, _ := p["base_url"].(string)
	return base
}

func TestCodexUpstreamAndEligibility(t *testing.T) {
	relay := config.Account{ID: "relay", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	put(t, filepath.Join(relay.CredentialsDir, "auth.json"), `{"OPENAI_API_KEY":"test-key"}`)
	if err := writeCodexProviderTOML(relay.CredentialsDir, "https://relay.example.com/v1", "responses"); err != nil {
		t.Fatal(err)
	}
	if base, ok := CodexUpstream(relay); !ok || base != "https://relay.example.com/v1" {
		t.Fatalf("relay upstream %q %v", base, ok)
	}
	official := config.Account{ID: "official", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	put(t, filepath.Join(official.CredentialsDir, "auth.json"), `{"OPENAI_API_KEY":"test-key"}`)
	if err := writeCodexProviderTOML(official.CredentialsDir, "", ""); err != nil {
		t.Fatal(err)
	}
	if base, ok := CodexUpstream(official); !ok || base != officialOpenAIBase {
		t.Fatalf("official upstream %q %v", base, ok)
	}
	official.Env = map[string]string{"OPENAI_BASE_URL": "https://env.example.com/v1"}
	if base, _ := CodexUpstream(official); base != "https://env.example.com/v1" {
		t.Fatalf("env upstream %q", base)
	}
	chatgpt := config.Account{ID: "chatgpt", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	put(t, filepath.Join(chatgpt.CredentialsDir, "auth.json"), `{"tokens":{"access_token":"a","refresh_token":"r","id_token":"i"}}`)
	if _, ok := CodexUpstream(chatgpt); ok {
		t.Fatal("ChatGPT login is eligible")
	}
	if _, ok := CodexUpstream(config.Account{ID: "claude", Type: config.AgentClaude, CredentialsDir: t.TempDir()}); ok {
		t.Fatal("Claude account treated as Codex")
	}
}

func TestApplyCodexEndpointRewritesSessionOnly(t *testing.T) {
	acct := config.Account{ID: "relay", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	put(t, filepath.Join(acct.CredentialsDir, "auth.json"), `{"OPENAI_API_KEY":"test-key"}`)
	if err := writeCodexProviderTOML(acct.CredentialsDir, "https://relay.example.com/v1", "chat"); err != nil {
		t.Fatal(err)
	}
	pool, _ := os.ReadFile(filepath.Join(acct.CredentialsDir, "config.toml"))
	bound := true
	svc, data := codexGatewayService(t, acct, func(config.Account, store.Session) string {
		if bound {
			return gatewayURL
		}
		return ""
	})
	// A seeded session: pool copy plus the session's own model.
	put(t, filepath.Join(codexHome(data), "config.toml"), "model = \"session-model\"\n"+string(pool))
	sess := store.Session{ID: "s1", User: "alice", Agent: config.AgentCodex, AccountID: acct.ID}
	if err := svc.ApplyCodexEndpoint(t.Context(), acct, sess); err != nil {
		t.Fatal(err)
	}
	home := readTOML(t, filepath.Join(codexHome(data), "config.toml"))
	if providerBase(home, "agentbox") != gatewayURL || home["model"] != "session-model" {
		t.Fatalf("session not routed through gateway: %v", home)
	}
	providers := home["model_providers"].(map[string]any)
	if providers["agentbox"].(map[string]any)["wire_api"] != "chat" {
		t.Fatal("wire_api changed")
	}
	if got, _ := os.ReadFile(filepath.Join(acct.CredentialsDir, "config.toml")); string(got) != string(pool) {
		t.Fatal("gateway URL leaked into the account pool")
	}
	// Unbinding restores the pool's real upstream.
	bound = false
	if err := svc.ApplyCodexEndpoint(t.Context(), acct, sess); err != nil {
		t.Fatal(err)
	}
	home = readTOML(t, filepath.Join(codexHome(data), "config.toml"))
	if providerBase(home, "agentbox") != "https://relay.example.com/v1" || home["model"] != "session-model" {
		t.Fatalf("unbinding did not restore the upstream: %v", home)
	}
	// A credential broadcast (key/URL change) keeps the gateway for bound sessions.
	bound = true
	put(t, filepath.Join(codexHome(data), "auth.json"), `{"OPENAI_API_KEY":"old-key"}`)
	if err := svc.SaveCodexKey(t.Context(), acct.ID, "new-key", "https://relay2.example.com/v1", "responses"); err != nil {
		t.Fatal(err)
	}
	home = readTOML(t, filepath.Join(codexHome(data), "config.toml"))
	if providerBase(home, "agentbox") != gatewayURL {
		t.Fatalf("broadcast dropped the gateway: %v", home)
	}
	if base, _ := ReadCodexProvider(acct.CredentialsDir); base != "https://relay2.example.com/v1" {
		t.Fatalf("pool upstream %q", base)
	}
	// Unseeded sessions are not created.
	other := store.Session{ID: "s9", User: "alice", Agent: config.AgentCodex, AccountID: acct.ID}
	if err := svc.ApplyCodexEndpoint(t.Context(), acct, other); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "users", "alice", "sessions", "s9")); !os.IsNotExist(err) {
		t.Fatal("created an unseeded session home")
	}
}

func TestCodexGatewayConfigForBuiltinProvider(t *testing.T) {
	raw := []byte("model_provider = \"openai\"\nprofile = \"p\"\n[profiles.p]\nmodel_provider = \"openai\"\nmodel = \"m\"\n")
	next, err := codexGatewayConfig(raw, gatewayURL)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{}
	if err := toml.Unmarshal(next, &settings); err != nil {
		t.Fatal(err)
	}
	if activeCodexProvider(settings) != gatewayProvider || providerBase(settings, gatewayProvider) != gatewayURL {
		t.Fatalf("built-in provider not replaced: %s", next)
	}
	p := settings["model_providers"].(map[string]any)[gatewayProvider].(map[string]any)
	if p["requires_openai_auth"] != true || p["wire_api"] != "responses" {
		t.Fatalf("provider fields: %v", p)
	}
	if !strings.Contains(string(next), "model = 'm'") && !strings.Contains(string(next), `model = "m"`) {
		t.Fatalf("profile model lost: %s", next)
	}
	if _, err := codexGatewayConfig([]byte("model_provider = \"ghost\"\n"), gatewayURL); err == nil {
		t.Fatal("undefined provider accepted")
	}
}
