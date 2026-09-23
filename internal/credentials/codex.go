package credentials

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"agentbox/internal/config"
)

// config.toml 里 provider 相关行的定位正则。只处理简单双引号字符串，
// 手写复杂 TOML（多行字符串等）不在覆盖范围。
var (
	reTomlBaseURL  = regexp.MustCompile(`(?m)^(\s*base_url\s*=\s*)"([^"]*)"`)
	reTomlWireAPI  = regexp.MustCompile(`(?m)^(\s*wire_api\s*=\s*)"([^"]*)"`)
	reTomlProvider = regexp.MustCompile(`(?m)^(model_provider\s*=\s*)"([^"]*)"`)
)

// tomlSet 把 re 匹配到的第一处赋值改为 val，保留行首前缀；no-op 当无匹配。
func tomlSet(text string, re *regexp.Regexp, val string) (string, bool) {
	loc := re.FindStringSubmatchIndex(text)
	if loc == nil {
		return text, false
	}
	prefix := text[loc[2]:loc[3]]
	return text[:loc[0]] + prefix + fmt.Sprintf("%q", val) + text[loc[1]:], true
}

// writeCodexProviderTOML 维护账号池 config.toml 的中转站配置。已有文件做
// 行级原地替换（保留手工调优的其余键），没有才生成最小模板。
func writeCodexProviderTOML(credDir, baseURL, wireAPI string) error {
	raw, err := readPoolFile(config.Account{CredentialsDir: credDir}, "config.toml")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		text = fmt.Sprintf(`model_provider = "agentbox"
disable_response_storage = true

[model_providers.agentbox]
name = "agentbox"
base_url = %q
wire_api = %q
requires_openai_auth = true
`, baseURL, wireAPI)
		return writePoolFile(config.Account{CredentialsDir: credDir}, "config.toml", []byte(text))
	}

	if next, ok := tomlSet(text, reTomlBaseURL, baseURL); ok {
		text = next
		if next, ok := tomlSet(text, reTomlWireAPI, wireAPI); ok {
			text = next
		} else {
			// 有 base_url 没 wire_api 的旧文件：紧跟 base_url 行补一条
			loc := reTomlBaseURL.FindStringIndex(text)
			text = text[:loc[1]] + fmt.Sprintf("\nwire_api = %q", wireAPI) + text[loc[1]:]
		}
	} else {
		// 完全没有 provider 段：追加模板段并让顶层 model_provider 指过去
		if next, ok := tomlSet(text, reTomlProvider, "agentbox"); ok {
			text = next
		} else {
			text = "model_provider = \"agentbox\"\n" + text
		}
		text = strings.TrimRight(text, "\n") + fmt.Sprintf(`

[model_providers.agentbox]
name = "agentbox"
base_url = %q
wire_api = %q
requires_openai_auth = true
`, baseURL, wireAPI)
	}
	return writePoolFile(config.Account{CredentialsDir: credDir}, "config.toml", []byte(text))
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
	if m := reTomlBaseURL.FindSubmatch(raw); m != nil {
		baseURL = string(m[2])
	}
	if m := reTomlWireAPI.FindSubmatch(raw); m != nil {
		wireAPI = string(m[2])
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
