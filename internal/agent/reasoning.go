package agent

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"agentbox/internal/config"
)

// TurnOptions has already been checked against the effective model policy.
// An empty Effort means inherit, never "disable reasoning".
type TurnOptions struct {
	Model        string
	Effort       string
	Control      string
	Unsupported  bool
	BudgetTokens int
}

func EffectiveReasoning(kind string, r *config.ReasoningCapability) config.ReasoningCapability {
	if r == nil {
		r = &config.ReasoningCapability{Support: "unknown"}
	}
	out := *config.CloneReasoning(r)
	if out.Control == "" && out.Support != "unsupported" {
		out.Control = "effort"
		if kind == config.AgentClaude {
			out.Control = "budget"
		}
	}
	return out
}

func ResolveTurnOptions(kind, model, effort, control string, capability *config.ReasoningCapability) (TurnOptions, error) {
	o := TurnOptions{Model: model, Effort: effort}
	if !modelRe.MatchString(model) {
		return o, fmt.Errorf("模型名 %q 无效", model)
	}
	r := EffectiveReasoning(kind, capability)
	o.Control, o.Unsupported = r.Control, r.Support == "unsupported"
	if effort == "" {
		return o, nil
	}
	if o.Unsupported {
		return o, fmt.Errorf("模型 %s 不支持调整推理强度，请恢复默认后重试", model)
	}
	// Pre-upgrade clients expressed Claude effort as a token budget. Do not
	// silently reinterpret a stale request as native effort after policy changes.
	if control == "" {
		control = EffectiveReasoning(kind, nil).Control
	}
	if control != r.Control {
		return o, fmt.Errorf("模型 %s 的推理设置已变更，请刷新并重新选择", model)
	}
	allowed := config.ReasoningLevels(kind, r.Control)
	if r.Support == "supported" {
		allowed = r.Levels
	}
	if !slices.Contains(allowed, effort) {
		return o, fmt.Errorf("模型 %s 不支持档位 %s，可选：%s", model, effort, strings.Join(allowed, "、"))
	}
	if r.Control == "budget" {
		o.BudgetTokens, _ = strconv.Atoi(claudeThinking[effort])
	}
	return o, nil
}
