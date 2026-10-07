package credentials

import (
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/config"
)

func TestConfigurationPresenceIsLocalAndPreservesCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, agent, file, data string
		env                     map[string]string
		want                    bool
	}{
		{"claude relay", config.AgentClaude, "", "", map[string]string{"ANTHROPIC_AUTH_TOKEN": "synthetic-key"}, true},
		{"claude api", config.AgentClaude, "", "", map[string]string{"ANTHROPIC_API_KEY": "synthetic-key"}, true},
		{"empty env", config.AgentClaude, "", "", map[string]string{"ANTHROPIC_API_KEY": "  "}, false},
		{"oauth expired still present", config.AgentClaude, ".credentials.json", `{"claudeAiOauth":{"accessToken":"synthetic-access","refreshToken":"synthetic-refresh","expiresAt":1}}`, nil, true},
		{"codex api", config.AgentCodex, "auth.json", `{"OPENAI_API_KEY":"synthetic-key"}`, nil, true},
		{"codex oauth", config.AgentCodex, "auth.json", `{"tokens":{"access_token":"synthetic-access"}}`, nil, true},
		{"missing", config.AgentCodex, "", "", nil, false},
		{"malformed", config.AgentCodex, "auth.json", `{"OPENAI_API_KEY":`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := config.Account{Type: tc.agent, CredentialsDir: dir, Env: tc.env}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if ConfigurationPresent(a) != tc.want {
				t.Fatal("wrong presence result")
			}
			if tc.file != "" {
				raw, err := os.ReadFile(filepath.Join(dir, tc.file))
				if err != nil || string(raw) != tc.data {
					t.Fatal("diagnosis mutated credentials")
				}
			}
		})
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(outside, []byte(`{"OPENAI_API_KEY":"synthetic-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if ConfigurationPresent(config.Account{Type: config.AgentCodex, CredentialsDir: dir}) {
		t.Fatal("followed credential symlink")
	}
}
