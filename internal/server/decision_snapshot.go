package server

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"agentbox/internal/agent"
	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
	"agentbox/internal/store"
)

// Each automatic role keeps its CLI, account and reasoning constraints. Model
// visibility is a picker preference, not authorization; an explicitly chosen
// hidden fallback remains eligible, just as it does in ordinary chat.
func decisionRoleSnapshot(cfg *config.Config, u store.User, mode string, fallback collaboration.Binding) (decision.RoleSnapshot, error) {
	out := decision.RoleSnapshot{Mode: mode, Binding: fallback, Fallback: fallback}
	if err := validateDecisionBinding(cfg, u, fallback); err != nil {
		return out, err
	}
	if mode == "fixed" {
		return out, nil
	}
	if mode != "auto" {
		return out, decision.ErrInvalid
	}
	a, _ := cfg.Account(fallback.AccountID)
	models := a.Models
	if len(models) == 0 {
		models = cfg.GetModels()[a.Type]
	}
	seen := map[string]bool{}
	prices := cfg.GetPricing()
	appendCandidate := func(model config.ModelOption) {
		if seen[model.ID] {
			return
		}
		b := fallback
		b.Model = model.ID
		if validateDecisionBinding(cfg, u, b) != nil {
			return
		}
		seen[model.ID] = true
		description := fmt.Sprintf("%s model %s", b.AgentType, b.Model)
		if model.Label != "" && model.Label != model.ID {
			description += ": " + boundedDecisionText(model.Label, 160)
		}
		price, priced := prices[b.Model]
		if !priced {
			price, priced = prices[b.AgentType]
		}
		if priced {
			description += fmt.Sprintf(". Configured reference USD per million tokens: uncached input %g, output %g", price.Input, price.Output)
			if price.Long != nil && price.LongContextOver > 0 {
				description += fmt.Sprintf("; over %d input tokens: input %g, output %g", price.LongContextOver, price.Long.Input, price.Long.Output)
			}
		}
		out.Candidates = append(out.Candidates, decision.Candidate{Ref: fmt.Sprintf("candidate_%d", len(out.Candidates)+1), Binding: b, Description: boundedDecisionText(description, 512)})
	}
	// The user's concrete selection is the fallback, even if an unrestricted
	// account's global suggestion list no longer contains that model.
	fallbackOption := config.ModelOption{ID: fallback.Model}
	for _, model := range models {
		if model.ID == fallback.Model {
			fallbackOption = model
			break
		}
	}
	appendCandidate(fallbackOption)
	for _, model := range models {
		appendCandidate(model)
	}
	if len(out.Candidates) == 0 {
		return out, decision.ErrInvalid
	}
	return out, nil
}

func validateDecisionBinding(cfg *config.Config, u store.User, b collaboration.Binding) error {
	if (collaboration.Roles{Coordinator: b, Executor: b}).Validate() != nil {
		return decision.ErrInvalid
	}
	a, ok := cfg.Account(b.AccountID)
	if !ok || a.Type != b.AgentType || !a.CanUse(u.Name, u.Role == store.RoleAdmin) || !a.AllowsModel(b.Model) {
		return errAccountAccess
	}
	_, err := agent.ResolveTurnOptions(b.AgentType, b.Model, b.Effort, b.EffortControl, cfg.ConfiguredReasoning(a, b.Model))
	return err
}

func boundedDecisionText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
	}
	return text
}

func decisionTaskState(in store.CollaborationCreateInput) string {
	// Only this request and its explicitly included thread context are eligible.
	// No source files, account metadata, home paths or other thread history.
	state := "Task:\n" + boundedDecisionText(in.Text, 12000)
	if in.Context != nil && in.Context.Include && strings.TrimSpace(in.Context.Text) != "" {
		state += "\nIncluded conversation:\n" + boundedDecisionText(in.Context.Text, 4000)
	}
	return state
}
