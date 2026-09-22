package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"agentbox/internal/config"
)

// SeedDefaultModel runs after credential seeding, which can copy an account's
// config over the workspace config. Only the model is replaced; provider,
// reasoning effort, MCP servers and other settings are retained.
func SeedDefaultModel(agentType, homeDir, model string, uid, gid int) error {
	if model == "" {
		return nil
	}
	if !modelRe.MatchString(model) {
		return fmt.Errorf("模型名 %q 无效", model)
	}
	var rel string
	switch agentType {
	case config.AgentClaude:
		rel = filepath.Join(".claude", "settings.json")
	case config.AgentCodex:
		rel = filepath.Join(".codex", "config.toml")
	default:
		return fmt.Errorf("unknown agent type %q", agentType)
	}
	// Session homes are writable by the container. Confine symlink resolution
	// to that home when reading and writing settings as the host service user.
	root, err := os.OpenRoot(homeDir)
	if err != nil {
		return err
	}
	defer root.Close()
	raw, err := root.ReadFile(rel)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	settings := map[string]any{}
	if len(raw) != 0 {
		if agentType == config.AgentClaude {
			err = json.Unmarshal(raw, &settings)
		} else {
			err = toml.Unmarshal(raw, &settings)
		}
		if err != nil {
			return fmt.Errorf("读取模型配置 %s: %w", rel, err)
		}
	}
	if settings == nil {
		settings = map[string]any{}
	}
	settings["model"] = model
	if agentType == config.AgentClaude {
		if env, ok := settings["env"].(map[string]any); ok {
			if _, present := env["ANTHROPIC_MODEL"]; present {
				env["ANTHROPIC_MODEL"] = model
			}
		}
		raw, err = json.MarshalIndent(settings, "", "  ")
	} else {
		// An active profile overrides top-level settings in Codex.
		if profiles, ok := settings["profiles"].(map[string]any); ok {
			if name, ok := settings["profile"].(string); ok {
				if profile, ok := profiles[name].(map[string]any); ok {
					profile["model"] = model
				}
			}
		}
		raw, err = toml.Marshal(settings)
	}
	if err != nil {
		return err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Close()
}
