package store

import (
	"agentbox/internal/collaboration"
	"testing"
	"time"
)

func TestCollaborationTimeBudgetAndWaitExpiry(t *testing.T) {
	s, u, r := preparedCollaboration(t, false)
	r = finishCollaborationFixture(t, s, u, r, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "one", Text: "Implement", Acceptance: []string{"test"}}}})
	if _, err := s.db.Exec("UPDATE collaborations SET updated_at=? WHERE id=?", usageTimestamp(time.Now().Add(-8*24*time.Hour)), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireCollaborationWaits(time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := s.Collaboration(u, r.ID)
	if err != nil || r.State != CollabPaused || r.WaitReason != "wait_expired" {
		t.Fatal(r, err)
	}
	r = applyCollaborationFixture(t, s, u, r, "resume", nil)
	if r.State != CollabAwaitingPlan {
		t.Fatal(r)
	}
	r = applyCollaborationFixture(t, s, u, r, "confirm_plan", map[string]any{"plan_version": r.PlanVersion})
	if _, err = s.db.Exec("UPDATE collaboration_steps SET started_at=?,ended_at=? WHERE run_id=? AND state='completed'", usageTimestamp(time.Now().Add(-7*time.Hour)), usageTimestamp(time.Now()), r.ID); err != nil {
		t.Fatal(err)
	}
	step := activeCollaborationStep(t, s, r.ID)
	if _, err = s.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision); err == nil {
		t.Fatal("runtime budget ignored")
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil || r.WaitReason != "runtime_limit" {
		t.Fatal(r, err)
	}
	cfg, err := s.CollaborationConfig(r.ID, r.ConfigVersion)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TotalRuntimeSeconds = 8 * 3600
	r = applyCollaborationFixture(t, s, u, r, "configure", cfg)
	if r.State != CollabAccepted {
		t.Fatal(r)
	}
	remaining, err := s.CollaborationRuntimeRemaining(u, r.ID)
	if err != nil || remaining > time.Hour+time.Second || remaining < time.Hour-time.Second {
		t.Fatal(remaining, err)
	}
}
