package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
	"agentbox/internal/store"
)

func TestCollaborationWorkflowChecksOnlyActiveBindings(t *testing.T) {
	s, source, h := clientTestServer(t)
	u, _ := s.store.GetUser(source.User)
	cfg := store.CollaborationConfig{
		Workflow: &collaboration.Workflow{Version: 1, Mode: "direct"},
		Roles: store.CollaborationRoles{
			Coordinator: collaboration.Binding{AgentType: "codex", AccountID: "unused", Model: "unused"},
			Executor:    collaboration.Binding{AgentType: "claude", AccountID: "shared", Model: "fixture"},
		},
	}
	if err := s.validateCollaborationBindings(u, cfg); err != nil {
		t.Fatal("unused participant blocked direct execution", err)
	}
	cfg.Workflow.Mode = "reviewed"
	if err := s.validateCollaborationBindings(u, cfg); !errors.Is(err, errAccountAccess) {
		t.Fatal("reviewer authorization was bypassed", err)
	}
	cfg.Workflow = nil
	if err := s.validateCollaborationBindings(u, cfg); !errors.Is(err, errAccountAccess) {
		t.Fatal("legacy reviewer authorization changed", err)
	}
	cfg.Workflow = &collaboration.Workflow{Version: 2, Mode: "direct"}
	if err := s.validateCollaborationBindings(u, cfg); err == nil {
		t.Fatal("unknown workflow version accepted")
	}
	if err := s.cfg.SetCollaborationEnabled(true); err != nil {
		t.Fatal(err)
	}
	w := clientRequest(h, u.Name, "GET", "/api/collaboration/options", "")
	var options struct {
		WorkflowVersion int `json:"workflow_version"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &options) != nil || options.WorkflowVersion != 1 {
		t.Fatal("workflow support is not advertised", w.Code, w.Body.String())
	}
}

func TestCollaborationWorkflowReplayCannotChangeStrategy(t *testing.T) {
	s, source, h := clientTestServer(t)
	u, _ := s.store.GetUser(source.User)
	if err := s.cfg.SetCollaborationEnabled(true); err != nil {
		t.Fatal(err)
	}
	b := collaboration.Binding{AgentType: "claude", AccountID: "shared", Model: "fixture"}
	in := store.CollaborationCreateInput{Workflow: &collaboration.Workflow{Version: 1, Mode: "direct"}, Scope: s.collaborationScope(u), SourceSessionID: source.ID, Text: "bounded task", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: collaboration.Roles{Coordinator: b, Executor: b}, MaxSteps: 10}
	cfg := store.CollaborationConfig{Workflow: in.Workflow, Roles: in.Roles, MaxSteps: in.MaxSteps}
	id := store.NewCollaborationID()
	if _, fresh, err := s.store.CreateCollaboration(u, id, in, cfg); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	request := func(input store.CollaborationCreateInput) int {
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return clientRequest(h, u.Name, "PUT", "/api/collaboration/runs/"+id, string(raw)).Code
	}
	if code := request(in); code != 200 {
		t.Fatal("exact replay failed", code)
	}
	in.Workflow = &collaboration.Workflow{Version: 1, Mode: "reviewed"}
	if code := request(in); code != 409 {
		t.Fatal("replay changed workflow", code)
	}
	in.Workflow = nil
	if code := request(in); code != 409 {
		t.Fatal("replay removed workflow", code)
	}
}

func TestDecisionDirectWorkflowRoutesOnlyExecutor(t *testing.T) {
	s, u, in, _ := decisionFixture(t)
	in.Workflow = &collaboration.Workflow{Version: 1, Mode: "direct"}
	in.Roles.Coordinator = collaboration.Binding{AgentType: "codex", AccountID: "unused", Model: "unused"}
	snapshot, err := s.collaborationDecisionSnapshot(u, in)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.CollaborationConfig{Workflow: in.Workflow, Roles: in.Roles, AutoExecute: in.AutoExecute, MaxSteps: in.MaxSteps, BudgetMicroUSD: in.BudgetMicroUSD}
	run, _, err := s.store.CreateCollaborationWithDecision(u, store.NewCollaborationID(), in, cfg, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.decisionProvider = func(config.DecisionJev) decision.Provider {
		return fixtureDecisionProvider(func(_ context.Context, req decision.Request) (decision.Evaluation, error) {
			calls.Add(1)
			if len(req.Roles) != 1 || len(req.Roles[collaboration.Executor]) != 2 {
				t.Error("inactive coordinator entered routing", req.Roles)
			}
			return knownDecision(req), nil
		})
	}
	if ready, err := s.resolveCollaborationDecision(t.Context(), u, run); err != nil || !ready {
		t.Fatal(ready, err)
	}
	record, err := s.store.CollaborationDecision(u, run.ID)
	if err != nil || record.Result == nil || record.Result.Roles.Executor.Model != "economy" || record.Result.Roles.Coordinator != in.Roles.Coordinator {
		t.Fatal(record, err)
	}
	if ready, err := s.resolveCollaborationDecision(t.Context(), u, run); err != nil || !ready || calls.Load() != 1 {
		t.Fatal("routing replay", ready, err, calls.Load())
	}
	in.Decision.Roles[collaboration.Coordinator] = decision.RoleIntent{Mode: "auto"}
	if _, err := s.collaborationDecisionSnapshot(u, in); err == nil {
		t.Fatal("inactive automatic role accepted")
	}
}
