package config

import "fmt"

func initialModels() map[string]string {
	return map[string]string{AgentCodex: "gpt-5.5", AgentClaude: "claude-opus-5"}
}

// defaultModels requires the caller to hold the config lock after startup.
func (c *Config) defaultModels() map[string]string {
	out := initialModels()
	for agent, model := range c.DefaultModels {
		out[agent] = model
	}
	return out
}

func (c *Config) initDefaultModels() {
	c.DefaultModels = c.defaultModels()
	// Existing installations may have a custom menu without the initial model.
	// Keep their entries and add the default so it can be selected and changed.
	for agent, model := range c.DefaultModels {
		found := false
		for _, option := range c.Models[agent] {
			found = found || option.ID == model
		}
		if !found {
			label := model
			switch model {
			case "gpt-5.5":
				label = "GPT-5.5"
			case "claude-opus-5":
				label = "Opus 5"
			}
			c.Models[agent] = append(c.Models[agent], ModelOption{ID: model, Label: label})
		}
	}
}

func (c *Config) validateDefaultModels() error {
	for agent, model := range c.DefaultModels {
		if agent != AgentClaude && agent != AgentCodex {
			return fmt.Errorf("default_models: unknown agent type %q", agent)
		}
		if !modelIDRe.MatchString(model) {
			return fmt.Errorf("default_models.%s: 模型 ID %q 无效", agent, model)
		}
		found := false
		for _, option := range c.Models[agent] {
			found = found || option.ID == model
		}
		if !found {
			return fmt.Errorf("请先将 %s 加入 %s 的模型列表；移除默认模型前请先选择其他默认模型", model, agent)
		}
	}
	return nil
}

func (c *Config) GetDefaultModels() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.defaultModels()
}

func (c *Config) GetDefaultModel(agent string) string {
	return c.GetDefaultModels()[agent]
}
