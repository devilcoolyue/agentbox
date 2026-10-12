package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"agentbox/internal/config"
	"agentbox/internal/safefs"
	"github.com/pelletier/go-toml/v2"
)

// SeedCollaborationHome runs on a stopped, dedicated participant after account
// credential convergence. It copies only login material and provider settings;
// templates, plugins, MCP commands and account shell hooks cannot leak into a
// collaboration by virtue of having been installed in an ordinary workspace.
func SeedCollaborationHome(kind, home, poolDir, model string, uid, gid int) error {
	if (kind != config.AgentClaude && kind != config.AgentCodex) || !modelRe.MatchString(model) {
		return errors.New("invalid collaboration agent configuration")
	}
	root, err := openHome(home)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range []string{".bashrc", ".bash_profile", ".profile", ".zshrc", ".agents", ".claude/skills", ".claude/plugins", ".claude/settings.local.json", ".codex/skills", ".codex/hooks.json"} {
		if err = root.RemoveAll(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	dir := ".codex"
	credential := "auth.json"
	if kind == config.AgentClaude {
		dir = ".claude"
		credential = ".credentials.json"
	}
	if err = root.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err = root.Chown(dir, uid, gid); err != nil {
		return err
	}
	settings := map[string]any{}
	if poolDir != "" {
		pool, err := safefs.Open(poolDir)
		if err != nil {
			return err
		}
		defer pool.Close()
		if _, err = pool.Lstat(credential); err == nil {
			if err = copyConfined(pool, credential, root, filepath.Join(dir, credential), uid, gid); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if kind == config.AgentCodex {
			raw, e := pool.ReadAll("config.toml", 4<<20)
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			if len(raw) > 0 {
				if toml.Unmarshal(raw, &settings) != nil {
					return errors.New("invalid account CLI configuration")
				}
			}
		}
	}
	if kind == config.AgentClaude {
		state, _ := json.Marshal(claudeSeedState)
		if err = writeOwned(root, ".claude.json", state, uid, gid); err != nil {
			return err
		}
		settings = map[string]any{"model": model, "disableAllHooks": true}
		raw, _ := json.Marshal(settings)
		return writeOwned(root, ".claude/settings.json", raw, uid, gid)
	}
	// An active account profile may override its provider. Preserve only the
	// selected connection and reasoning defaults, never the profile's tools.
	if name, ok := settings["profile"].(string); ok {
		if profiles, ok := settings["profiles"].(map[string]any); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				for _, key := range []string{"model_provider", "model_reasoning_effort"} {
					if value, ok := profile[key]; ok {
						settings[key] = value
					}
				}
			}
		}
	}
	allowed := map[string]any{"model": model}
	for _, key := range []string{"model_provider", "forced_login_method", "forced_chatgpt_workspace_id", "cli_auth_credentials_store", "model_reasoning_effort"} {
		if value, ok := settings[key]; ok {
			allowed[key] = value
		}
	}
	if providers, ok := settings["model_providers"].(map[string]any); ok {
		provider, _ := allowed["model_provider"].(string)
		if value, ok := providers[provider].(map[string]any); ok {
			connection := map[string]any{}
			for _, key := range []string{"name", "base_url", "env_key", "wire_api", "http_headers", "env_http_headers", "query_params", "requires_openai_auth", "request_max_retries", "stream_max_retries", "stream_idle_timeout_ms"} {
				if v, ok := value[key]; ok {
					connection[key] = v
				}
			}
			allowed["model_providers"] = map[string]any{provider: connection}
		}
	}
	raw, err := toml.Marshal(allowed)
	if err != nil {
		return err
	}
	return writeOwned(root, ".codex/config.toml", raw, uid, gid)
}

// ValidateCollaborationProject rejects unverified project-level executable
// configuration. Instruction markdown stays usable. Supporting project MCP,
// hooks or skill plugins requires an explicit tested task policy later.
func ValidateCollaborationProject(root *safefs.Root) error {
	forbidden := []string{".mcp.json", ".claude/settings.json", ".claude/settings.local.json", ".claude/skills", ".claude/agents", ".claude/plugins", ".agents/skills", ".codex/config.toml", ".codex/hooks.json", ".codex/skills"}
	pending := []string{"."}
	remaining := 10000
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := root.ReadDirLimit(dir, max(1, remaining))
		if err != nil {
			return err
		}
		remaining -= len(entries)
		if remaining < 0 {
			return errors.New("project entry limit exceeded")
		}
		for _, entry := range entries {
			name := filepath.ToSlash(filepath.Join(dir, entry.Name()))
			if (entry.Name() == ".claude" || entry.Name() == ".codex" || entry.Name() == ".agents") && !entry.IsDir() {
				return errors.New("project CLI configuration aliases are not supported")
			}
			for _, denied := range forbidden {
				if name == denied || strings.HasSuffix(name, "/"+denied) {
					return errors.New("project CLI extensions require an explicit collaboration policy")
				}
			}
			if entry.IsDir() && name != ".git" {
				if strings.Count(name, "/") >= 64 {
					return errors.New("project directory depth exceeded")
				}
				pending = append(pending, name)
			}
			// Do not follow symlink aliases. Snapshot validation separately
			// rejects escaping links; every actual directory is inspected.
		}
	}
	return nil
}
