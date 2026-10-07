package credentials

import (
	"encoding/json"
	"strings"

	"agentbox/internal/config"
)

// ConfigurationPresent is a local, bounded presence check, not authentication
// or model authorization. It never refreshes, syncs or contacts a provider.
func ConfigurationPresent(a config.Account) bool {
	if a.Type == config.AgentClaude {
		if strings.TrimSpace(a.Env["ANTHROPIC_AUTH_TOKEN"]) != "" || strings.TrimSpace(a.Env["ANTHROPIC_API_KEY"]) != "" {
			return true
		}
		raw, err := readPoolFile(a, ".credentials.json")
		if err != nil {
			return false
		}
		var c struct {
			OAuth struct {
				Access  string `json:"accessToken"`
				Refresh string `json:"refreshToken"`
			} `json:"claudeAiOauth"`
		}
		return json.Unmarshal(raw, &c) == nil && (strings.TrimSpace(c.OAuth.Access) != "" || strings.TrimSpace(c.OAuth.Refresh) != "")
	}
	if a.Type == config.AgentCodex {
		if strings.TrimSpace(a.Env["OPENAI_API_KEY"]) != "" {
			return true
		}
		auth, err := readCodexAuth(a)
		return err == nil && (strings.TrimSpace(auth.APIKey) != "" || strings.TrimSpace(auth.Tokens.AccessToken) != "" || strings.TrimSpace(auth.Tokens.RefreshToken) != "")
	}
	return false
}

func Status(a config.Account) (status string, expiresAt int64) {
	if a.CredentialsDir == "" {
		return "missing", 0
	}
	switch a.Type {
	case config.AgentClaude:
		raw, err := readPoolFile(a, ".credentials.json")
		if err != nil {
			return "missing", 0
		}
		var c struct {
			ClaudeAiOauth struct {
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(raw, &c) != nil || c.ClaudeAiOauth.RefreshToken == "" {
			return "norefresh", 0
		}
		return "ok", c.ClaudeAiOauth.ExpiresAt
	case config.AgentCodex:
		auth, err := readCodexAuth(a)
		if err != nil {
			return "missing", 0
		}
		if auth.APIKey != "" {
			return "ok", 0
		}
		if auth.Tokens.AccessToken == "" {
			return "missing", 0
		}
		if auth.Tokens.RefreshToken == "" {
			return "norefresh", 0
		}
		return "ok", 0
	}
	return "missing", 0
}
