package credentials

import (
	"encoding/json"
	"os"

	"agentbox/internal/config"
	"github.com/pelletier/go-toml/v2"
)

// writeCodexProviderTOML 更新账号池的连接配置，保留模型、推理强度和 MCP。
func writeCodexProviderTOML(credDir, baseURL, wireAPI string) error {
	raw, err := readPoolFile(config.Account{CredentialsDir: credDir}, "config.toml")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	settings := map[string]any{}
	if err := toml.Unmarshal(raw, &settings); err != nil {
		return err
	}
	if len(settings) == 0 {
		settings["disable_response_storage"] = true
	}
	provider := "openai"
	if baseURL != "" {
		provider = "agentbox"
	}
	settings["model_provider"] = provider
	settings["forced_login_method"] = "api"
	settings["cli_auth_credentials_store"] = "file"
	delete(settings, "forced_chatgpt_workspace_id")
	providers, _ := settings["model_providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		settings["model_providers"] = providers
	}
	delete(providers, "openai")
	if baseURL != "" {
		providers["agentbox"] = map[string]any{"name": "agentbox", "base_url": baseURL, "wire_api": wireAPI, "requires_openai_auth": true}
	}
	if profiles, ok := settings["profiles"].(map[string]any); ok {
		if name, ok := settings["profile"].(string); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				profile["model_provider"] = provider
				delete(profile, "forced_login_method")
				delete(profile, "forced_chatgpt_workspace_id")
				delete(profile, "cli_auth_credentials_store")
			}
		}
	}
	next, err := toml.Marshal(settings)
	if err != nil {
		return err
	}
	return writePoolFile(config.Account{CredentialsDir: credDir}, "config.toml", next)
}

// ReadCodexProvider 从账号池 config.toml 读当前 base_url / wire_api，供
// 前端弹窗预填。文件缺失或没配返回空串。
func ReadCodexProvider(credDir string) (baseURL, wireAPI string) {
	if credDir == "" {
		return "", ""
	}
	raw, err := readPoolFile(config.Account{CredentialsDir: credDir}, "config.toml")
	if err != nil {
		return "", ""
	}
	var settings map[string]any
	if toml.Unmarshal(raw, &settings) != nil {
		return "", ""
	}
	provider, _ := settings["model_provider"].(string)
	if profiles, ok := settings["profiles"].(map[string]any); ok {
		if name, ok := settings["profile"].(string); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				if p, ok := profile["model_provider"].(string); ok {
					provider = p
				}
			}
		}
	}
	if providers, ok := settings["model_providers"].(map[string]any); ok {
		if selected, ok := providers[provider].(map[string]any); ok {
			baseURL, _ = selected["base_url"].(string)
			wireAPI, _ = selected["wire_api"].(string)
		}
	}
	return baseURL, wireAPI
}

// SavedAPIKey 读账号池 auth.json 里已保存的 key，探测时留空复用。
func SavedAPIKey(credDir string) string {
	if credDir == "" {
		return ""
	}
	raw, err := readPoolFile(config.Account{CredentialsDir: credDir}, "auth.json")
	if err != nil {
		return ""
	}
	var a struct {
		Key string `json:"OPENAI_API_KEY"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	return a.Key
}
