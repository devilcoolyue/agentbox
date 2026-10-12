package store

import (
	"errors"
	"testing"
	"time"

	"agentbox/internal/collaboration"
)

func TestCollaborationBudgetChecksCommittedRowsBeforeNextClaim(t *testing.T) {
	for _, scenario := range []string{"within", "exhausted", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			s, u, r := preparedCollaboration(t, true)
			if _, err := s.db.Exec("UPDATE collaborations SET budget_micro_usd=100 WHERE id=?", r.ID); err != nil {
				t.Fatal(err)
			}
			first := activeCollaborationStep(t, s, r.ID)
			r = finishCollaborationFixture(t, s, u, r, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "one", Text: "Implement", Acceptance: []string{"test"}}}})
			if scenario != "unknown" {
				cost := int64(50)
				if scenario == "exhausted" {
					cost = 100
				}
				if err := s.InsertUsage(UsageEvent{User: u.Name, SessionID: "planner", ThreadID: r.ID, TurnID: first.TurnID, Agent: "codex", Model: "planner", Kind: UsageKindChat, CostMicroUSD: cost, Price: &PriceSnapshot{Version: 1, Source: "table"}, TS: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			step := activeCollaborationStep(t, s, r.ID)
			_, err := s.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision)
			if scenario == "within" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, collaboration.ErrState) {
					t.Fatal(err)
				}
				r, e := s.Collaboration(u, r.ID)
				if e != nil || r.State != CollabPaused {
					t.Fatal(r, e)
				}
				want := "budget_limit"
				if scenario == "unknown" {
					want = "usage_incomplete"
				}
				if r.WaitReason != want {
					t.Fatal(r)
				}
				st, e := s.CollaborationStep(step.ID)
				if e != nil || st.State != "accepted" || st.Revision != step.Revision {
					t.Fatal("budget gate claimed model work", st, e)
				}
			}
		})
	}
}

func TestCollaborationWorkFreezesInputAndProviderIdentity(t *testing.T) {
	s, u, r := preparedCollaboration(t, false)
	step := activeCollaborationStep(t, s, r.ID)
	step, err := s.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision)
	if err != nil {
		t.Fatal(err)
	}
	step, err = s.AdvanceCollaborationStep(u, r.ID, step.ID, step.Revision, "running")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCollaborationProviderSession(u, r.ID, step.ID, step.Revision, "original-thread"); err != nil {
		t.Fatal(err)
	}
	w, err := s.CollaborationWork(u, r.ID)
	if err != nil || w.Session.ChatSession != "original-thread" || w.Binding.Model != "planner" || w.Input.Text != "Plan" {
		t.Fatal(w, err)
	}
	if err = s.SaveCollaborationProviderSession(u, r.ID, step.ID, step.Revision, "different-thread"); !errors.Is(err, ErrChatConflict) {
		t.Fatal("provider identity changed", err)
	}
	if _, err = s.db.Exec("UPDATE collaboration_steps SET input_json=? WHERE id=?", `{"text":"tampered","snapshot_id":""}`, step.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CollaborationWork(u, r.ID); !errors.Is(err, ErrChatConflict) {
		t.Fatal("modified frozen input accepted", err)
	}
}
