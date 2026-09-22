package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestSeedDefaultModelPreservesAccountSettings(t *testing.T) {
	for _, agentType := range []string{"claude", "codex"} {
		t.Run(agentType, func(t *testing.T) {
			home, pool := t.TempDir(), t.TempDir()
			name, model := "settings.json", "claude-opus-5"
			body := `{"model":"old","effortLevel":"high","env":{"ANTHROPIC_MODEL":"old","KEEP":"yes"},"statusLine":{"type":"command","command":"custom-hud"}}`
			if agentType == "codex" {
				name, model = "config.toml", "gpt-5.5"
				body = `model = "old"
model_reasoning_effort = "xhigh"
model_provider = "relay"
profile = "work"
[profiles.work]
model = "profile-old"
model_reasoning_effort = "high"
[model_providers.relay]
base_url = "https://example.com/v1"
wire_api = "responses"
[mcp_servers.tools]
command = "node"
args = ["tools.js"]
`
			}
			if err := os.WriteFile(filepath.Join(pool, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			// Account config gets re-copied on restart; the workspace model must win
			// every time without changing the shared account or other CLI settings.
			for range 2 {
				if err := SeedCredentials(agentType, home, pool, os.Getuid(), os.Getgid()); err != nil {
					t.Fatal(err)
				}
				if err := SeedDefaultModel(agentType, home, model, os.Getuid(), os.Getgid()); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(home, "."+agentType, name))
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if agentType == "claude" {
					err = json.Unmarshal(raw, &got)
				} else {
					err = toml.Unmarshal(raw, &got)
				}
				if err != nil || got["model"] != model {
					t.Fatalf("seeded settings = %v, error = %v", got, err)
				}
				if agentType == "claude" {
					env := got["env"].(map[string]any)
					if got["effortLevel"] != "high" || env["KEEP"] != "yes" || env["ANTHROPIC_MODEL"] != model || got["statusLine"].(map[string]any)["command"] != "custom-hud" {
						t.Fatalf("Claude settings lost: %v", got)
					}
				} else {
					profile := got["profiles"].(map[string]any)["work"].(map[string]any)
					provider := got["model_providers"].(map[string]any)["relay"].(map[string]any)
					if got["model_reasoning_effort"] != "xhigh" || got["model_provider"] != "relay" || profile["model"] != model || profile["model_reasoning_effort"] != "high" || provider["base_url"] != "https://example.com/v1" || got["mcp_servers"] == nil {
						t.Fatalf("Codex settings lost: %v", got)
					}
				}
			}
			if raw, err := os.ReadFile(filepath.Join(pool, name)); err != nil || string(raw) != body {
				t.Fatal("account pool settings were changed")
			}
		})
	}
}

func TestSeedDefaultModelPreservesInvalidConfig(t *testing.T) {
	for _, agentType := range []string{"claude", "codex"} {
		t.Run(agentType, func(t *testing.T) {
			home := t.TempDir()
			name := "settings.json"
			if agentType == "codex" {
				name = "config.toml"
			}
			path := filepath.Join(home, "."+agentType, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("invalid config {"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := SeedDefaultModel(agentType, home, "some-model", os.Getuid(), os.Getgid()); err == nil {
				t.Fatal("invalid settings were silently overwritten")
			}
			if raw, _ := os.ReadFile(path); string(raw) != "invalid config {" {
				t.Fatal("invalid settings were changed")
			}
		})
	}
}

func TestSeedDefaultModelRejectsOutsideSymlink(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(outside, []byte("model = 'keep'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".codex", "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaultModel("codex", home, "gpt-5.5", os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("accepted config outside session home")
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "model = 'keep'\n" {
		t.Fatal("modified file outside session home")
	}
}
