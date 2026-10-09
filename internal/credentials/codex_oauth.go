package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"agentbox/internal/config"
	"github.com/pelletier/go-toml/v2"
)

type CodexOAuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id,omitempty"`
}

type codexAuth struct {
	Mode        string           `json:"auth_mode,omitempty"`
	APIKey      string           `json:"OPENAI_API_KEY,omitempty"`
	Tokens      CodexOAuthTokens `json:"tokens"`
	LastRefresh string           `json:"last_refresh,omitempty"`
}

func readCodexAuth(acct config.Account) (codexAuth, error) {
	var auth codexAuth
	raw, err := readPoolFile(acct, "auth.json")
	if err == nil {
		err = json.Unmarshal(raw, &auth)
	}
	return auth, err
}

func CodexAuthMode(acct config.Account) string {
	raw, err := readPoolFile(acct, "auth.json")
	if err != nil {
		return ""
	}
	return codexAuthMode(raw)
}

// The ID token is received directly from the trusted token endpoint. Decode
// only the account selector; these claims never authorize an agentbox user.
func codexAccountID(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

// Select the official provider in both the root and active profile. Preserve
// model, reasoning, MCP and dormant profiles; force file storage for bind mounts.
func codexLoginConfig(raw []byte, mode string) ([]byte, error) {
	settings := map[string]any{}
	if err := toml.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("读取 Codex 配置: %w", err)
	}
	settings["model_provider"] = "openai"
	settings["cli_auth_credentials_store"] = "file"
	settings["forced_login_method"] = mode
	delete(settings, "forced_chatgpt_workspace_id")
	if providers, ok := settings["model_providers"].(map[string]any); ok {
		delete(providers, "openai")
	}
	if profiles, ok := settings["profiles"].(map[string]any); ok {
		if name, ok := settings["profile"].(string); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				profile["model_provider"] = "openai"
				delete(profile, "forced_login_method")
				delete(profile, "forced_chatgpt_workspace_id")
				delete(profile, "cli_auth_credentials_store")
			}
		}
	}
	return toml.Marshal(settings)
}

func (s *Service) SaveCodexOAuth(ctx context.Context, id string, tokens CodexOAuthTokens) error {
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.IDToken == "" {
		return fmt.Errorf("OAuth 凭证不完整")
	}
	release, err := s.Lock(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	acct, ok := s.cfg.Account(id)
	if !ok || acct.Type != config.AgentCodex {
		return fmt.Errorf("Codex account no longer exists")
	}
	rawConfig, err := readPoolFile(acct, "config.toml")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	nextConfig, err := codexLoginConfig(rawConfig, "chatgpt")
	if err != nil {
		return err
	}
	tokens.AccountID = codexAccountID(tokens.IDToken)
	raw, err := json.MarshalIndent(codexAuth{Mode: "chatgpt", Tokens: tokens, LastRefresh: time.Now().UTC().Format(time.RFC3339)}, "", "  ")
	if err != nil {
		return err
	}
	// Switch provider before exposing subscription credentials to the CLI.
	if err := writePoolFile(acct, "config.toml", nextConfig); err != nil {
		return err
	}
	env := make(map[string]string, len(acct.Env))
	for k, v := range acct.Env {
		switch k {
		case "OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY":
			continue
		}
		env[k] = v
	}
	if len(env) != len(acct.Env) {
		if _, err := s.cfg.UpdateAccount(id, config.AccountPatch{Env: &env}); err != nil {
			return err
		}
	}
	if err := writePoolFile(acct, "auth.json", raw); err != nil {
		return err
	}
	s.broadcast(acct)
	return nil
}

// Overlay only connection settings on a session's config, preserving its model
// and MCP settings. The active session profile must not override the account.
func codexConnectionConfig(raw, pool []byte) ([]byte, error) {
	settings, source := map[string]any{}, map[string]any{}
	if err := toml.Unmarshal(raw, &settings); err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(pool, &source); err != nil {
		return nil, err
	}
	for _, key := range []string{"model_provider", "forced_login_method", "cli_auth_credentials_store"} {
		if value, ok := source[key]; ok {
			settings[key] = value
		} else {
			delete(settings, key)
		}
	}
	delete(settings, "forced_chatgpt_workspace_id")
	provider, _ := source["model_provider"].(string)
	providers, _ := settings["model_providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		settings["model_providers"] = providers
	}
	delete(providers, "openai")
	if poolProviders, ok := source["model_providers"].(map[string]any); ok {
		if selected, ok := poolProviders[provider]; ok {
			providers[provider] = selected
		}
	}
	if profiles, ok := settings["profiles"].(map[string]any); ok {
		if name, ok := settings["profile"].(string); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				profile["model_provider"] = provider
				for _, key := range []string{"forced_login_method", "forced_chatgpt_workspace_id", "cli_auth_credentials_store"} {
					delete(profile, key)
				}
			}
		}
	}
	return toml.Marshal(settings)
}

func codexAuthMode(raw []byte) string {
	var auth codexAuth
	if json.Unmarshal(raw, &auth) != nil {
		return ""
	}
	if auth.APIKey != "" {
		return "apikey"
	}
	if auth.Tokens.AccessToken != "" {
		return "oauth"
	}
	return ""
}

// CodexAccess returns the pool's ChatGPT access token after pulling a newer
// one back from the account's workspaces. It never refreshes: the Codex CLI
// owns that rotation chain, and a second refresher would invalidate it.
func (s *Service) CodexAccess(ctx context.Context, acct config.Account) (accessToken, accountID string, err error) {
	release, err := s.Lock(ctx, acct.ID)
	if err != nil {
		return "", "", err
	}
	defer release()
	current, ok := s.cfg.Account(acct.ID)
	if !ok {
		return "", "", fmt.Errorf("account no longer exists")
	}
	s.syncAcctCreds(current)
	auth, err := readCodexAuth(current)
	if err != nil || auth.Tokens.AccessToken == "" {
		return "", "", fmt.Errorf("账号尚未完成订阅登录")
	}
	accountID = auth.Tokens.AccountID
	if accountID == "" {
		accountID = codexAccountID(auth.Tokens.IDToken)
	}
	return auth.Tokens.AccessToken, accountID, nil
}
