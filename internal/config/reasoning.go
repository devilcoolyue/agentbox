package config

import (
	"fmt"
	"slices"
)

// ReasoningCapability is an explicit policy, not an assertion that an upstream
// provider implements the option. Nil means unknown; an explicit unknown policy
// also suppresses lower-priority discovery (useful for relays).
type ReasoningCapability struct {
	Support string   `json:"support"`
	Control string   `json:"control,omitempty"`
	Levels  []string `json:"levels,omitempty"`
}

func ReasoningLevels(agent, control string) []string {
	if agent == AgentClaude {
		if control == "effort" {
			return []string{"low", "medium", "high", "xhigh", "max"}
		}
		return []string{"low", "medium", "high", "xhigh"}
	}
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

func ValidateReasoning(agent string, r *ReasoningCapability) error {
	if r == nil {
		return nil
	}
	if r.Support != "unknown" && r.Support != "supported" && r.Support != "unsupported" {
		return fmt.Errorf("reasoning.support 必须为 unknown、supported 或 unsupported")
	}
	if r.Support == "unsupported" {
		if r.Control != "" || len(r.Levels) != 0 {
			return fmt.Errorf("不支持调整的模型不能配置 control / levels")
		}
		return nil
	}
	if r.Control != "effort" && r.Control != "budget" && !(r.Support == "unknown" && r.Control == "") {
		return fmt.Errorf("reasoning.control 必须为 effort 或 budget")
	}
	if agent == AgentCodex && r.Control == "budget" {
		return fmt.Errorf("Codex 不支持思考预算模式")
	}
	if r.Support == "supported" && len(r.Levels) == 0 {
		return fmt.Errorf("支持调整的模型必须指定 levels")
	}
	if r.Support == "unknown" && len(r.Levels) != 0 {
		return fmt.Errorf("未知模型不能声明支持的 levels")
	}
	seen := map[string]bool{}
	for _, level := range r.Levels {
		if !slices.Contains(ReasoningLevels(agent, r.Control), level) || seen[level] {
			return fmt.Errorf("无效或重复的推理档位 %q", level)
		}
		seen[level] = true
	}
	return nil
}

func CloneReasoning(r *ReasoningCapability) *ReasoningCapability {
	if r == nil {
		return nil
	}
	out := *r
	out.Levels = slices.Clone(r.Levels)
	return &out
}

func cloneReasoningMap(in map[string]ReasoningCapability) map[string]ReasoningCapability {
	if in == nil {
		return nil
	}
	out := make(map[string]ReasoningCapability, len(in))
	for id, r := range in {
		out[id] = *CloneReasoning(&r)
	}
	return out
}

// CloneModelOptions deep-copies a model list so callers may annotate it.
func CloneModelOptions(in []ModelOption) []ModelOption { return cloneModelOptions(in) }

func cloneModelOptions(in []ModelOption) []ModelOption {
	out := slices.Clone(in)
	for i := range out {
		out[i].Reasoning = CloneReasoning(out[i].Reasoning)
	}
	return out
}

func cloneModels(in map[string][]ModelOption) map[string][]ModelOption {
	out := make(map[string][]ModelOption, len(in))
	for key, models := range in {
		out[key] = cloneModelOptions(models)
	}
	return out
}

// ConfiguredReasoning resolves exact IDs only. Model aliases and relay names
// are not reliable evidence of capabilities. Priority: the account's override,
// the account's own model list, then the global model list.
func (c *Config) ConfiguredReasoning(a Account, model string) *ReasoningCapability {
	if r, ok := a.ModelReasoning[model]; ok {
		return CloneReasoning(&r)
	}
	for _, m := range a.Models {
		if m.ID == model && m.Reasoning != nil {
			return CloneReasoning(m.Reasoning)
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.Models[a.Type] {
		if m.ID == model {
			return CloneReasoning(m.Reasoning)
		}
	}
	return nil
}
