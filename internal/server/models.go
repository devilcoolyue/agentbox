package server

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sort"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/credentials"
	"agentbox/internal/store"
)

type sessionModelsView struct {
	Models           []config.ModelOption       `json:"models"`
	DefaultReasoning config.ReasoningCapability `json:"default_reasoning"`
	Discovery        string                     `json:"discovery"` // available, unavailable, stopped
	// Restricted: the account has its own model list; only these models are
	// accepted and no custom ID can be entered. DefaultModel is the model a
	// turn without a choice uses.
	Restricted   bool   `json:"restricted"`
	DefaultModel string `json:"default_model"`
	// ClientEffort is the native effort the account's CLI configuration uses
	// when a turn names none (Codex model_reasoning_effort, Claude
	// CLAUDE_CODE_EFFORT_LEVEL). The page shows and sends it as the default
	// level of models that accept it.
	ClientEffort string `json:"client_effort,omitempty"`
}

func accountClientEffort(acct config.Account) string {
	effort := acct.Env["CLAUDE_CODE_EFFORT_LEVEL"]
	if acct.Type == config.AgentCodex {
		effort = credentials.ReadCodexEffort(acct.CredentialsDir)
	}
	if !slices.Contains(config.ReasoningLevels(acct.Type, "effort"), effort) {
		return ""
	}
	return effort
}

// Metadata requests never wake stopped workspaces. No model inference occurs.
func (s *Server) discoverReasoning(ctx context.Context, sess store.Session, acct config.Account) (map[string]config.ReasoningCapability, string) {
	if sess.Status != store.StatusRunning || sess.ContainerID == "" || s.dock == nil {
		return nil, "stopped"
	}
	if sess.Agent != config.AgentCodex {
		return nil, "unavailable"
	}
	// Even the openai provider may be redirected by an environment override.
	if acct.Env["OPENAI_BASE_URL"] != "" {
		return nil, "unavailable"
	}
	env, err := s.execEnv(sess)
	if err != nil {
		return nil, "unavailable"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stream, err := s.dock.ExecStream(ctx, sess.ContainerID, []string{"codex", "app-server"}, env)
	if err != nil {
		return nil, "unavailable"
	}
	defer stream.Close()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); err := stream.Demux(pw, io.Discard); pw.CloseWithError(err) }()
	defer func() { pr.Close(); stream.Close(); <-done }()
	models, err := agent.ProbeCodexModels(stream, pr)
	if err != nil {
		return nil, "unavailable"
	}
	return models, "available"
}

func (s *Server) handleSessionModels(w http.ResponseWriter, r *http.Request, sess store.Session) {
	acct, err := s.sessionAccount(sess)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	discovered, state := s.discoverReasoning(r.Context(), sess, acct)
	view := sessionModelsView{DefaultReasoning: agent.EffectiveReasoning(sess.Agent, nil), Discovery: state,
		Restricted: acct.RestrictsModels(), DefaultModel: acct.ResolveModel(sess.DefaultModel), ClientEffort: accountClientEffort(acct)}
	if view.Restricted {
		view.Models = config.CloneModelOptions(acct.Models)
	} else {
		view.Models = s.cfg.GetModels()[sess.Agent]
		seen := map[string]bool{}
		for _, m := range view.Models {
			seen[m.ID] = true
		}
		// Account-only overrides and the workspace default remain selectable.
		accountModels := make([]string, 0, len(acct.ModelReasoning))
		for model := range acct.ModelReasoning {
			accountModels = append(accountModels, model)
		}
		sort.Strings(accountModels)
		for _, model := range accountModels {
			if !seen[model] {
				view.Models = append(view.Models, config.ModelOption{ID: model, Label: model})
				seen[model] = true
			}
		}
		if !seen[sess.DefaultModel] && sess.DefaultModel != "" {
			view.Models = append(view.Models, config.ModelOption{ID: sess.DefaultModel, Label: sess.DefaultModel})
		}
	}
	for i := range view.Models {
		m := &view.Models[i]
		capability := s.cfg.ConfiguredReasoning(acct, m.ID)
		if capability == nil {
			if value, ok := discovered[m.ID]; ok {
				capability = &value
			}
		}
		value := agent.EffectiveReasoning(sess.Agent, capability)
		m.Reasoning = &value
	}
	// Recheck withdrawal that happened while probing; no other account metadata
	// (provider URLs, environment, grants) is present in this response.
	if _, err := s.sessionAccount(sess); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}
