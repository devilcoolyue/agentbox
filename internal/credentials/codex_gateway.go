package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"github.com/pelletier/go-toml/v2"
)

// Codex reads its endpoint from the session's .codex/config.toml, not from the
// exec env. To route an extension-bound API key / relay account through the
// model gateway, the session copy gets a session-specific base_url while the
// account pool keeps the real upstream: the pool is shared by every session of
// the account, and the gateway URL authenticates one session.

// officialOpenAIBase is where Codex's built-in "openai" provider goes when
// neither the config nor OPENAI_BASE_URL names another endpoint.
const officialOpenAIBase = "https://api.openai.com/v1"

// gatewayProvider names the provider written for accounts that used the
// built-in one; it matches the provider writeCodexProviderTOML creates.
const gatewayProvider = "agentbox"

// activeCodexProvider returns the provider the CLI will use: the active
// profile's choice wins over the top-level one.
func activeCodexProvider(settings map[string]any) string {
	provider, _ := settings["model_provider"].(string)
	if profiles, ok := settings["profiles"].(map[string]any); ok {
		if name, ok := settings["profile"].(string); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				if p, ok := profile["model_provider"].(string); ok && p != "" {
					provider = p
				}
			}
		}
	}
	return provider
}

// CodexUpstream is the real endpoint of an API key / relay Codex account, read
// from the pool. ok is false for ChatGPT-login accounts and for configurations
// the gateway cannot stand in for.
func CodexUpstream(acct config.Account) (baseURL string, ok bool) {
	if acct.Type != config.AgentCodex || CodexAuthMode(acct) != "apikey" {
		return "", false
	}
	raw, err := readPoolFile(acct, "config.toml")
	if err != nil && !os.IsNotExist(err) {
		return "", false
	}
	settings := map[string]any{}
	if toml.Unmarshal(raw, &settings) != nil {
		return "", false
	}
	switch provider := activeCodexProvider(settings); provider {
	case "", "openai":
		if v := acct.Env["OPENAI_BASE_URL"]; v != "" {
			return v, true
		}
		return officialOpenAIBase, true
	default:
		providers, _ := settings["model_providers"].(map[string]any)
		selected, _ := providers[provider].(map[string]any)
		base, _ := selected["base_url"].(string)
		return base, base != ""
	}
}

// codexGatewayConfig points the active provider of a session config at the
// gateway. Accounts on the built-in provider get an equivalent custom provider,
// since the built-in one cannot be given a base_url in config.
func codexGatewayConfig(raw []byte, gatewayURL string) ([]byte, error) {
	settings := map[string]any{}
	if err := toml.Unmarshal(raw, &settings); err != nil {
		return nil, err
	}
	providers, _ := settings["model_providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		settings["model_providers"] = providers
	}
	provider := activeCodexProvider(settings)
	if provider == "" || provider == "openai" {
		provider = gatewayProvider
		providers[provider] = map[string]any{"name": provider, "base_url": gatewayURL, "wire_api": "responses", "requires_openai_auth": true}
		settings["model_provider"] = provider
		if profiles, ok := settings["profiles"].(map[string]any); ok {
			if name, ok := settings["profile"].(string); ok {
				if profile, ok := profiles[name].(map[string]any); ok {
					profile["model_provider"] = provider
				}
			}
		}
		return toml.Marshal(settings)
	}
	selected, ok := providers[provider].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Codex provider %q has no configuration", provider)
	}
	next := make(map[string]any, len(selected))
	for k, v := range selected {
		next[k] = v
	}
	next["base_url"] = gatewayURL
	providers[provider] = next
	return toml.Marshal(settings)
}

// SetCodexEndpoint installs the function that returns a session's gateway
// base URL, or "" when its Codex traffic goes straight to the account's
// upstream. Call it before the service is shared.
func (s *Service) SetCodexEndpoint(fn func(config.Account, store.Session) string) {
	s.codexEndpoint = fn
}

// codexSessionConfig derives a session's config.toml: the pool's connection
// settings over the session's own (model, MCP, profiles), then the gateway
// override when the session is routed through it.
func (s *Service) codexSessionConfig(acct config.Account, sess store.Session, homeRaw, poolRaw []byte) ([]byte, error) {
	next, err := codexConnectionConfig(homeRaw, poolRaw)
	if err != nil {
		return nil, err
	}
	if s.codexEndpoint == nil {
		return next, nil
	}
	if gateway := s.codexEndpoint(acct, sess); gateway != "" {
		return codexGatewayConfig(next, gateway)
	}
	return next, nil
}

// ApplyCodexEndpoint brings one seeded session's Codex connection in line with
// the current extension bindings: the gateway URL when bound, the pool's real
// upstream otherwise. Unchanged files are not rewritten, so the CLI and the
// mtime-based template merge see no churn. A session without a seeded config
// is left alone.
func (s *Service) ApplyCodexEndpoint(ctx context.Context, acct config.Account, sess store.Session) error {
	if acct.Type != config.AgentCodex || sess.Agent != config.AgentCodex || acct.CredentialsDir == "" {
		return nil
	}
	release, err := s.Lock(ctx, acct.ID)
	if err != nil {
		return err
	}
	defer release()
	home, err := s.openHome(sess)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer home.Close()
	current, err := home.ReadAll(".codex/config.toml", credentialMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pool, err := readPoolFile(acct, "config.toml")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	next, err := s.codexSessionConfig(acct, sess, current, pool)
	if err != nil {
		return err
	}
	if bytes.Equal(next, current) {
		return nil
	}
	_, err = home.WriteFile(".codex/config.toml", next, safefs.WriteOptions{Mode: 0600, Chown: true, UID: dockerx.AgentUID, GID: dockerx.AgentGID, BestEffortChown: true})
	return err
}
