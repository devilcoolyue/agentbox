package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"agentbox/internal/config"
	"agentbox/internal/safefs"
)

// ClearCredentials undoes the account-specific part of SeedCredentials so a
// stopped session home can be bound to another account: the rotating login
// file, Claude's cached account profile and the Codex provider connection.
// Model, MCP servers, skills and conversation history stay in place.
//
// The rotating file has to be removed rather than left for the next start to
// overwrite: credential sync runs before seeding, and a newer token from the
// previous account would be adopted into the next account's pool.
func ClearCredentials(agentType, homeDir string, uid, gid int) error {
	if agentType != config.AgentClaude && agentType != config.AgentCodex {
		return fmt.Errorf("unknown agent type %q", agentType)
	}
	root, err := openHome(homeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // never prepared: nothing was seeded
	}
	if err != nil {
		return err
	}
	defer root.Close()
	_, homeRel := RotatingCredFile(agentType)
	if err := root.Remove(filepath.FromSlash(homeRel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if agentType == config.AgentClaude {
		return clearClaudeAccount(root, uid, gid)
	}
	return clearCodexConnection(root, uid, gid)
}

// Claude Code caches the signed-in account (email, organization) in
// ~/.claude.json. A fresh home has no such entry and the CLI fills it in from
// the credentials it is given, so dropping it restores that state.
func clearClaudeAccount(root *safefs.Root, uid, gid int) error {
	raw, err := root.ReadAll(".claude.json", 64<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep large integers exactly as the CLI wrote them
	var state map[string]any
	if dec.Decode(&state) != nil || state == nil {
		return nil // not ours to repair; the CLI reports a broken state file itself
	}
	if _, ok := state["oauthAccount"]; !ok {
		return nil
	}
	delete(state, "oauthAccount")
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		return err
	}
	return writeOwned(root, ".claude.json", out.Bytes(), uid, gid)
}

// codexConnectionKeys are the config.toml keys the account pool owns
// (see credentials.writeCodexProviderTOML).
var codexConnectionKeys = []string{"model_provider", "forced_login_method", "forced_chatgpt_workspace_id", "cli_auth_credentials_store"}

// Seeding replaces config.toml whenever the next account's pool has one. Drop
// the connection keys anyway so a pool without it cannot inherit the previous
// account's relay endpoint.
func clearCodexConnection(root *safefs.Root, uid, gid int) error {
	rel := filepath.Join(".codex", "config.toml")
	raw, err := root.ReadAll(rel, 4<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	settings := map[string]any{}
	if toml.Unmarshal(raw, &settings) != nil {
		return nil // SeedDefaultModel reports an unreadable config on start
	}
	for _, key := range codexConnectionKeys {
		delete(settings, key)
	}
	if providers, ok := settings["model_providers"].(map[string]any); ok {
		delete(providers, "agentbox")
		delete(providers, "openai")
		if len(providers) == 0 {
			delete(settings, "model_providers")
		}
	}
	if profiles, ok := settings["profiles"].(map[string]any); ok {
		if name, ok := settings["profile"].(string); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				for _, key := range codexConnectionKeys {
					delete(profile, key)
				}
			}
		}
	}
	next, err := toml.Marshal(settings)
	if err != nil {
		return err
	}
	return writeOwned(root, rel, next, uid, gid)
}
