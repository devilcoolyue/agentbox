package credentials

import (
	"encoding/json"

	"agentbox/internal/config"
)

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
