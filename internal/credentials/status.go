package credentials

import (
	"bytes"
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
		raw, err := readPoolFile(a, "auth.json")
		if err != nil || !bytes.Contains(raw, []byte("OPENAI_API_KEY")) {
			return "missing", 0
		}
		return "ok", 0
	}
	return "missing", 0
}
