package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"agentbox/internal/config"
	"agentbox/internal/safefs"
)

func writePoolFile(acct config.Account, name string, raw []byte) error {
	if acct.CredentialsDir == "" {
		return fmt.Errorf("账号未配置 credentials_dir")
	}
	if err := os.MkdirAll(acct.CredentialsDir, 0700); err != nil {
		return err
	}
	root, err := safefs.Open(acct.CredentialsDir)
	if err != nil {
		return err
	}
	defer root.Close()
	_, err = root.WriteFile(name, raw, safefs.WriteOptions{Mode: 0600})
	return err
}
func (s *Service) SaveClaude(ctx context.Context, id string, raw []byte) error {
	release, err := s.Lock(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	acct, ok := s.cfg.Account(id)
	if !ok || acct.Type != config.AgentClaude {
		return fmt.Errorf("Claude account no longer exists")
	}
	if err := writePoolFile(acct, ".credentials.json", raw); err != nil {
		return err
	}
	s.broadcast(acct)
	return nil
}
func (s *Service) SaveCodexKey(ctx context.Context, id, key, baseURL, wireAPI string) error {
	release, err := s.Lock(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	acct, ok := s.cfg.Account(id)
	if !ok || acct.Type != config.AgentCodex {
		return fmt.Errorf("Codex account no longer exists")
	}
	raw, err := json.MarshalIndent(map[string]string{"OPENAI_API_KEY": key}, "", "  ")
	if err != nil {
		return err
	}
	// Each file publishes atomically. This is not a transaction across the pair,
	// matching the existing API's partial failure behavior.
	if err := writePoolFile(acct, "auth.json", raw); err != nil {
		return err
	}
	if baseURL != "" {
		return writeCodexProviderTOML(acct.CredentialsDir, baseURL, wireAPI)
	}
	return nil
}
