package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
)

const poolLogin = `{"claudeAiOauth":{"accessToken":"pool-access","refreshToken":"pool-refresh","expiresAt":1792906943276,"subscriptionType":"max"},"mcpOAuth":{"x":{"accessToken":"pool-extra"}}}`

func TestSeedCredentialsScopesServerRefreshedLogin(t *testing.T) {
	home, pool := t.TempDir(), t.TempDir()
	write := func(path, raw string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pool, ".credentials.json"), poolLogin)
	write(filepath.Join(pool, ServerRefreshMarker), "x\n")
	target := filepath.Join(home, ".claude", ".credentials.json")
	write(target, `{"claudeAiOauth":{"accessToken":"stale"},"mcpOAuth":{"mine":{"accessToken":"session-mcp"}}}`)

	if err := SeedCredentials(config.AgentClaude, home, pool, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Login map[string]any            `json:"claudeAiOauth"`
		MCP   map[string]map[string]any `json:"mcpOAuth"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Login["accessToken"] != "pool-access" || got.Login["refreshToken"] != nil {
		t.Fatalf("login: %v", got.Login)
	}
	if got.MCP["mine"] == nil || got.MCP["x"] != nil {
		t.Fatalf("session MCP entries must be kept and pool ones not copied: %v", got.MCP)
	}
	if !strings.Contains(string(raw), "1792906943276") {
		t.Fatalf("expiresAt not preserved exactly: %s", raw)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", ServerRefreshMarker)); !os.IsNotExist(err) {
		t.Fatalf("marker copied into home: %v", err)
	}
}

func TestSeedCredentialsLegacyPoolCopiesVerbatim(t *testing.T) {
	home, pool := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(pool, ".credentials.json"), []byte(poolLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedCredentials(config.AgentClaude, home, pool, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if string(raw) != poolLogin {
		t.Fatalf("unmarked pool must keep the old behavior until migration: %s", raw)
	}
}

func TestClaudeHomeCredential(t *testing.T) {
	if _, _, err := ClaudeHomeCredential([]byte(`{"other":1}`), nil); err == nil {
		t.Fatal("pool without a login accepted")
	}
	out, changed, err := ClaudeHomeCredential([]byte(poolLogin), []byte("not json"))
	if err != nil || !changed || strings.Contains(string(out), "refresh") || strings.Contains(string(out), "pool-extra") {
		t.Fatalf("broken home: %s %v %v", out, changed, err)
	}
	again, changed, err := ClaudeHomeCredential([]byte(poolLogin), out)
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("not idempotent: %v %v", changed, err)
	}
}
