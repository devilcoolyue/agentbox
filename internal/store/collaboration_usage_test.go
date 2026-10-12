package store

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
)

func TestCollaborationUsageAggregatesOnlyOriginalStepLedger(t *testing.T) {
	s, u, run := preparedCollaboration(t, true)
	plan := activeCollaborationStep(t, s, run.ID)
	base := UsageEvent{TS: time.Now(), User: u.Name, SessionID: "planner", ThreadID: run.ID, TurnID: plan.TurnID, Kind: UsageKindChat, Agent: "codex", Model: "planner", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80, CacheWriteTokens: 10, CostMicroUSD: 50, Price: &PriceSnapshot{Version: 1, Source: "table"}}
	if err := s.InsertUsage(base); err != nil {
		t.Fatal(err)
	}
	second := base
	second.Model, second.InputTokens, second.OutputTokens, second.CacheReadTokens, second.CacheWriteTokens, second.CostMicroUSD = "tool-model", 2, 3, 4, 5, 7
	if err := s.InsertUsage(second); err != nil {
		t.Fatal(err)
	}
	run = finishCollaborationFixture(t, s, u, run, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "one", Text: "Implement", Acceptance: []string{"test"}}}})
	cost, steps, err := s.CollaborationUsage(u, run.ID)
	if err != nil || len(steps) != 2 || cost.CostMicroUSD != 57 {
		t.Fatal(cost, steps, err)
	}
	got := steps[0]
	if got.StepID != plan.ID || got.Status != "settled" || *got.InputTokens != 102 || *got.OutputTokens != 23 || *got.CacheReadTokens != 84 || *got.CacheWriteTokens != 15 || *got.TotalTokens != 224 || *got.CostMicroUSD != 57 || !reflect.DeepEqual(got.Models, []string{"planner", "tool-model"}) {
		t.Fatalf("incorrect per-turn accounting: %+v", got)
	}
	if steps[1].Status != "pending" || steps[1].TotalTokens != nil || steps[1].CostMicroUSD != nil {
		t.Fatal("unstarted step invented zero usage", steps[1])
	}
	// Matching IDs alone are not enough: exclude another user, thread, original
	// participant session, and title/decision accounting from model-step usage.
	for _, change := range []func(*UsageEvent){
		func(e *UsageEvent) { e.User = "bob" },
		func(e *UsageEvent) { e.ThreadID = "foreign-run" },
		func(e *UsageEvent) { e.SessionID = "other-session" },
		func(e *UsageEvent) { e.Kind = UsageKindTitle },
		func(e *UsageEvent) { e.Kind = UsageKindDecision },
	} {
		foreign := base
		change(&foreign)
		if err = s.InsertUsage(foreign); err != nil {
			t.Fatal(err)
		}
	}
	_, after, err := s.CollaborationUsage(u, run.ID)
	if err != nil || !reflect.DeepEqual(after, steps) {
		t.Fatal("foreign or auxiliary usage leaked into step", after, err)
	}
	foreign := u
	foreign.Name = "bob"
	if _, _, err = s.CollaborationUsage(foreign, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign owner read usage", err)
	}
	foreign = u
	foreign.CreatedAt = foreign.CreatedAt.Add(time.Hour)
	if _, _, err = s.CollaborationUsage(foreign, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("new user identity inherited usage", err)
	}
}

func TestCollaborationUsageDistinguishesMissingUnpricedAndRecordedZero(t *testing.T) {
	for _, state := range []string{"accepted", "starting", "running", "finalizing", "completed", "uncertain", "reviewed", "failed", "interrupted", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s, u, run := preparedCollaboration(t, false)
			step := activeCollaborationStep(t, s, run.ID)
			if _, err := s.db.Exec("UPDATE collaboration_steps SET state=? WHERE id=?", state, step.ID); err != nil {
				t.Fatal(err)
			}
			_, steps, err := s.CollaborationUsage(u, run.ID)
			pending := state == "accepted" || state == "starting" || state == "running" || state == "finalizing"
			want := "unavailable"
			if pending {
				want = "pending"
			}
			if err != nil || len(steps) != 1 || steps[0].Status != want || steps[0].InputTokens != nil || steps[0].OutputTokens != nil || steps[0].CacheReadTokens != nil || steps[0].CacheWriteTokens != nil || steps[0].TotalTokens != nil || steps[0].CostMicroUSD != nil {
				t.Fatal("missing evidence became zero", steps, err)
			}
			ev := UsageEvent{TS: time.Now(), User: u.Name, SessionID: "planner", ThreadID: run.ID, TurnID: step.TurnID, Kind: UsageKindChat, Agent: "codex", Model: "planner", Price: &PriceSnapshot{Version: 1, Source: "table"}}
			if err = s.InsertUsage(ev); err != nil {
				t.Fatal(err)
			}
			_, steps, err = s.CollaborationUsage(u, run.ID)
			want = "partial"
			if state == "completed" {
				want = "settled"
			}
			if err != nil || steps[0].Status != want || steps[0].TotalTokens == nil || *steps[0].TotalTokens != 0 || steps[0].CostMicroUSD == nil || *steps[0].CostMicroUSD != 0 {
				t.Fatal("observed zero was lost", steps, err)
			}
			ev.Price.Source, ev.Model, ev.InputTokens = "unpriced", "unpriced-model", 1
			if err = s.InsertUsage(ev); err != nil {
				t.Fatal(err)
			}
			_, steps, err = s.CollaborationUsage(u, run.ID)
			if err != nil || steps[0].Status != "partial" || *steps[0].TotalTokens != 1 || steps[0].CostMicroUSD != nil {
				t.Fatal("unpriced row was presented as a known zero fee", steps, err)
			}
		})
	}
}

func TestCollaborationUsageDoesNotRecountClaudeMessageReplays(t *testing.T) {
	s, u, run := preparedCollaboration(t, true)
	run = finishCollaborationFixture(t, s, u, run, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "one", Text: "Implement", Acceptance: []string{"test"}}}})
	step := activeCollaborationStep(t, s, run.ID)
	ev := UsageEvent{TS: time.Now(), User: u.Name, SessionID: "builder", ThreadID: run.ID, TurnID: step.TurnID, Kind: UsageKindChat, Agent: "claude", Model: "builder", InputTokens: 13, OutputTokens: 5, CacheReadTokens: 17, CacheWriteTokens: 7, CostMicroUSD: 42, ReqID: "message-1", Raw: `{"type":"synthetic"}`, Price: &PriceSnapshot{Version: 1, Source: "table", PerRequest: true}}
	for i := 0; i < 2; i++ {
		if _, err := s.InsertUsageMessages(ev); err != nil {
			t.Fatal(err)
		}
	}
	_, rows, err := s.CollaborationUsage(u, run.ID)
	if err != nil || len(rows) != 2 || *rows[1].TotalTokens != 42 || *rows[1].CostMicroUSD != 42 {
		t.Fatal("message replay counted twice", rows, err)
	}
	before := len(s.ListUsage(UsageFilter{User: u.Name}))
	for i := 0; i < 3; i++ {
		if _, _, err = s.CollaborationUsage(u, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.ListUsage(UsageFilter{User: u.Name})) != before {
		t.Fatal("reading usage wrote ledger rows")
	}
}

func TestDecisionAttemptUsageRequiresEvidenceAndRemainsSeparate(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	run, d := acceptDecision(t, s, u, in, cfg, snap)
	attempt, _, err := s.BeginCollaborationDecisionAttempt(u, run.ID, d.Revision, NewCollaborationID(), "mindshub", config.DecisionJevModel, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []decision.Usage{{CostStatus: "unknown"}, {Known: true, InputTokens: 11, OutputTokens: 2, CostStatus: "unknown"}, {Known: true, InputTokens: 11, OutputTokens: 2, CostStatus: "known"}} {
		if err = s.SettleCollaborationDecisionAttempt(u, run.ID, attempt.ID, evidence, config.DecisionJevModel); err != nil {
			t.Fatal(err)
		}
		got, err := s.CollaborationDecision(u, run.ID)
		if err != nil || got.Attempt.TokensKnown != evidence.Known {
			t.Fatal("decision unknown token evidence became zero", got, err)
		}
		cost, steps, err := s.CollaborationUsage(u, run.ID)
		if err != nil || len(steps) != 0 || cost.DecisionCalls != 1 || cost.DecisionPartial != (evidence.CostStatus != "known") {
			t.Fatal("decision accounting entered model steps", cost, steps, err)
		}
	}
	if _, err = s.db.Exec("UPDATE decision_attempts SET evidence_json='{}' WHERE id=?", attempt.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.CollaborationDecision(u, run.ID)
	if err != nil || got.Attempt.TokensKnown {
		t.Fatal("legacy evidence manufactured known token totals", got, err)
	}
}
