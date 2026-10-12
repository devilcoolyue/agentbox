package store

import (
	"agentbox/internal/collaboration"
	"strings"
	"testing"
)

func TestCollaborationHandoffRepairPreservesWorkAndRole(t *testing.T) {
	s, u, r := preparedCollaboration(t, true)
	r = finishCollaborationFixture(t, s, u, r, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "one", Text: "Implement", Acceptance: []string{"test"}}}})
	step := activeCollaborationStep(t, s, r.ID)
	step, err := s.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "finalizing"} {
		step, err = s.AdvanceCollaborationStep(u, r.ID, step.ID, step.Revision, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot := strings.Repeat("e", 32)
	if _, err = s.db.Exec("INSERT INTO collaboration_artifacts(id,run_id,step_id,kind,storage_key,sha256,size_bytes,state,created_at) VALUES(?,?,?,'snapshot',?,'hash',1,'ready','fixture')", snapshot, r.ID, step.ID, "snapshots/"+snapshot); err != nil {
		t.Fatal(err)
	}
	r, err = s.CompleteCollaborationStep(u, r.ID, step.ID, step.Revision, CollaborationCompletion{Final: []byte("I implemented the change."), SnapshotID: snapshot, HistorySynced: true, UsageSettled: true, ProcessStopped: true})
	if err != nil || r.WaitReason != "handoff_invalid" {
		t.Fatal(r, err)
	}
	r = applyCollaborationFixture(t, s, u, r, "repair_handoff", nil)
	w, err := s.CollaborationWork(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Step.Kind != "handoff_repair" || w.Step.Role != "executor" || w.Input.RepairKind != "execute" || w.Step.SnapshotID != snapshot || !strings.Contains(w.Input.Text, "Do not repeat implementation") {
		t.Fatal(w)
	}
	step, err = s.ClaimCollaborationStep(u, r.ID, w.Step.ID, w.Step.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "finalizing"} {
		step, err = s.AdvanceCollaborationStep(u, r.ID, step.ID, step.Revision, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	final := []byte(`{"version":1,"type":"result","payload":{"summary":"Already implemented","completed":["one"],"tests":[]}}`)
	r, err = s.CompleteCollaborationStep(u, r.ID, step.ID, step.Revision, CollaborationCompletion{Final: final, SnapshotID: snapshot, HistorySynced: true, UsageSettled: true, ProcessStopped: true})
	if err != nil || r.WaitReason != "verification_required" {
		t.Fatal(r, err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM collaboration_steps WHERE run_id=? AND kind='execute'", r.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("implementation repeated", count, err)
	}
}

func TestCollaborationDeleteKeepsFenceAndAccounting(t *testing.T) {
	s, u, r := preparedCollaboration(t, false)
	if err := s.DeleteCollaboration(u, r.ID, r.Scope, r.Revision); err == nil {
		t.Fatal("deleted active task")
	}
	r = applyCollaborationFixture(t, s, u, r, "pause", nil)
	if err := s.DeleteCollaboration(u, r.ID, "foreign", r.Revision); err == nil {
		t.Fatal("foreign scope")
	}
	if err := s.DeleteCollaboration(u, r.ID, r.Scope, r.Revision); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.Collaboration(u, r.ID)
	if err != nil || deleted.State != CollabDeleted || deleted.Request != nil {
		t.Fatal(deleted, err)
	}
	if err = s.DeleteCollaboration(u, r.ID, r.Scope, r.Revision); err != nil {
		t.Fatal("cleanup not retryable", err)
	}
	if _, _, err = s.CreateCollaboration(u, r.RequestID, *r.Request, CollaborationConfig{Roles: r.Request.Roles}); err == nil {
		t.Fatal("deleted creation replayed")
	}
	participants, err := s.CollaborationParticipants(u, r.ID)
	if err != nil || len(participants) != 2 {
		t.Fatal("cleanup lost participant identity", err)
	}
	var text string
	if err = s.db.QueryRow("SELECT input_json FROM collaboration_steps WHERE run_id=?", r.ID).Scan(&text); err != nil || text != "null" {
		t.Fatal("content retained", text, err)
	}
}
