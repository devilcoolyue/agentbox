package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"agentbox/internal/config"
	"github.com/pelletier/go-toml/v2"
)

// CheckReasoningInheritance never rewrites CLI configuration. It rejects known
// conflicting overrides rather than silently promising that omission disables
// reasoning. CLI/model defaults may still apply when no explicit override exists.
func CheckReasoningInheritance(kind, home, workspace string, env map[string]string, o TurnOptions) error {
	if !o.Unsupported && (o.Effort == "" || kind != config.AgentClaude) {
		return nil
	}
	conflict := func(source string) error {
		return fmt.Errorf("模型 %s 的推理设置与 %s 冲突；请清除对应 CLI 设置，或在模型能力中选择实际支持的调整方式后重试", o.Model, source)
	}
	badKey := func(key string) bool {
		if o.Unsupported {
			return key == "model_reasoning_effort" || key == "effortLevel" || key == "MAX_THINKING_TOKENS" || key == "CLAUDE_CODE_EFFORT_LEVEL"
		}
		if o.Control == "budget" {
			return key == "effortLevel" || key == "CLAUDE_CODE_EFFORT_LEVEL"
		}
		return key == "MAX_THINKING_TOKENS" || key == "CLAUDE_CODE_EFFORT_LEVEL" || key == "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING"
	}
	for key, value := range env {
		if value != "" && badKey(key) {
			// Native --effort clears the process budget without changing the account.
			if !o.Unsupported && o.Control == "effort" && (key == "MAX_THINKING_TOKENS" || key == "CLAUDE_CODE_EFFORT_LEVEL" || key == "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING") {
				continue
			}
			return conflict("账号环境变量 " + key)
		}
	}
	type location struct{ dir, file string }
	files := []location{{home, ".codex/config.toml"}, {workspace, ".codex/config.toml"}}
	if kind == config.AgentClaude {
		files = []location{{home, ".claude/settings.json"}, {workspace, ".claude/settings.json"}, {workspace, ".claude/settings.local.json"}}
	}
	for _, file := range files {
		root, err := openHome(file.dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("无法检查 CLI 推理配置: %w", err)
		}
		raw, err := root.ReadAll(file.file, 4<<20)
		root.Close()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("无法检查 %s 推理配置: %w", file.file, err)
		}
		values := map[string]any{}
		if strings.HasSuffix(file.file, ".toml") {
			err = toml.Unmarshal(raw, &values)
		} else {
			err = json.Unmarshal(raw, &values)
		}
		if err != nil {
			return fmt.Errorf("无法解析 %s 推理配置", file.file)
		}
		sections := []map[string]any{values}
		if profiles, ok := values["profiles"].(map[string]any); ok {
			name, _ := values["profile"].(string)
			if profile, ok := profiles[name].(map[string]any); ok {
				sections = append(sections, profile)
			}
		}
		if models, ok := values["modelSettings"].(map[string]any); ok {
			if model, ok := models[o.Model].(map[string]any); ok {
				sections = append(sections, model)
			}
		}
		if env, ok := values["env"].(map[string]any); ok {
			sections = append(sections, env)
		}
		for _, section := range sections {
			for key, value := range section {
				if value != nil && value != "" && badKey(key) {
					return conflict(file.file + " 的 " + key)
				}
			}
		}
	}
	if o.Unsupported && kind == config.AgentClaude {
		// The verified Claude CLI emits both adaptive thinking and a default
		// output_config.effort even for unrecognized models. There is no verified
		// per-turn omission option that preserves all other model capabilities.
		return fmt.Errorf("模型 %s 标记为不支持推理调整，但当前 Claude Code 仍可能自动添加 thinking / effort，无法保证安全省略；请使用兼容该模型的 CLI / 中转配置，验证后再更新模型能力", o.Model)
	}
	return nil
}
