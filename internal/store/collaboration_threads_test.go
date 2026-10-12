package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCollaborationThreadAtomicBindingAndSupplement(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	in.Context = &CollaborationThreadContext{ThreadID: "thread", Anchor: 2, Include: true, SHA256: strings.Repeat("a", 64), Text: "User: prior"}
	r, f, err := s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg)
	if err != nil || !f {
		t.Fatal(err)
	}
	if _, f, err = s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg); err != nil || f {
		t.Fatal("replay", err)
	}
	if _, _, err = s.CreateCollaboration(u, strings.Repeat("b", 32), in, cfg); !errors.Is(err, ErrChatConflict) {
		t.Fatal("multiple active thread tasks", err)
	}
	rows, err := s.CollaborationThreadRuns(u, in.SourceSessionID, "thread")
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Request.PromptGoal(), "User: prior") {
		t.Fatal(rows, err)
	}
	// A paused, unstarted run can accept requirements; reconfiguration preserves
	// its original request fingerprint and adds a new immutable configuration.
	if _, err = s.db.Exec("UPDATE collaborations SET state='paused',wait_reason='user_paused' WHERE id=?", r.ID); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"text": "also include tests"})
	action := CollaborationActionInput{Scope: in.Scope, ExpectedRevision: r.Revision, Action: "supplement", Payload: payload}
	receipt, f, err := s.ApplyCollaborationAction(u, r.ID, strings.Repeat("c", 32), action)
	if err != nil || !f {
		t.Fatal(receipt, err)
	}
	if _, f, err = s.ApplyCollaborationAction(u, r.ID, strings.Repeat("c", 32), action); err != nil || f {
		t.Fatal("duplicate supplement", err)
	}
	rebuilt, err := s.IncorporateCollaborationSupplements(u, r.ID)
	if err != nil || !rebuilt {
		t.Fatal(err)
	}
	updated, _ := s.Collaboration(u, r.ID)
	if updated.ConfigVersion != 2 || updated.State != CollabAccepted || updated.Request.Text != in.Text {
		t.Fatal(updated)
	}
	rebuilt, err = s.IncorporateCollaborationSupplements(u, r.ID)
	if err != nil || rebuilt {
		t.Fatal("double incorporation", err)
	}
	if _, f, err = s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg); err != nil || f {
		t.Fatal("original frozen request changed", err)
	}
}

func TestCollaborationApplicationReceiptAndFrozenChatContext(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	in.Context = &CollaborationThreadContext{ThreadID: "thread", Anchor: 1, SHA256: strings.Repeat("a", 64)}
	r, _, err := s.CreateCollaboration(u, strings.Repeat("b", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Repeat("c", 32)
	if _, err = s.db.Exec("UPDATE collaborations SET state='completed',snapshot_id=? WHERE id=?", snapshot, r.ID); err != nil {
		t.Fatal(err)
	}
	action := strings.Repeat("d", 32)
	a, f, err := s.BeginCollaborationApplication(u, r.ID, action, snapshot, r.Revision)
	if err != nil || !f || a.State != "staging" {
		t.Fatal(a, err)
	}
	if _, f, err = s.BeginCollaborationApplication(u, r.ID, action, snapshot, r.Revision); err != nil || f {
		t.Fatal("application replay", err)
	}
	if blocked, err := s.CollaborationApplicationBlocked(in.SourceSessionID); err != nil || !blocked {
		t.Fatal(blocked, err)
	}
	if err = s.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	a, err = s.CollaborationApplication(u, r.ID)
	if err != nil || a.State != "uncertain" {
		t.Fatal(a, err)
	}
	if err = s.SetCollaborationApplication(u, r.ID, action, "uncertain", "failed"); err != nil {
		t.Fatal(err)
	}
	if blocked, err := s.CollaborationApplicationBlocked(in.SourceSessionID); err != nil || blocked {
		t.Fatal(blocked, err)
	}
	c, _, err := s.AcceptChatRequest(u, in.SourceSessionID, "chat-request", ChatRequestInput{Scope: "chat", ThreadID: "thread", Text: "continue", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	material, err := s.ChatCollaborationContext(u, in.SourceSessionID, c.RequestID)
	if err != nil || !strings.Contains(material, snapshot) {
		t.Fatal(material, err)
	}
	s.db.Exec("UPDATE collaborations SET snapshot_id=? WHERE id=?", strings.Repeat("e", 32), r.ID)
	again, err := s.ChatCollaborationContext(u, in.SourceSessionID, c.RequestID)
	if err != nil || again != material {
		t.Fatal("context changed after acceptance", err)
	}
	s.db.Exec("UPDATE collaborations SET state='deleted' WHERE id=?", r.ID)
	again, err = s.ChatCollaborationContext(u, in.SourceSessionID, c.RequestID)
	if err != nil || again != "" {
		t.Fatal("deleted context disclosed", err)
	}
}

func TestCollaborationApplicationAbandonFencesLateRequest(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	r, _, err := s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Repeat("b", 32)
	s.db.Exec("UPDATE collaborations SET state='completed',snapshot_id=? WHERE id=?", snapshot, r.ID)
	old, newID := strings.Repeat("c", 32), strings.Repeat("d", 32)
	if err = s.AbandonCollaborationApplication(u, r.ID, old, snapshot); err != nil {
		t.Fatal(err)
	}
	a, f, err := s.BeginCollaborationApplication(u, r.ID, old, snapshot, r.Revision)
	if err != nil || f || a.State != "failed" {
		t.Fatal("late request admitted", a, f, err)
	}
	a, f, err = s.BeginCollaborationApplication(u, r.ID, newID, snapshot, r.Revision)
	if err != nil || !f {
		t.Fatal("explicit new application", a, f, err)
	}
	if err = s.SetCollaborationApplication(u, r.ID, newID, "staging", "switching"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCollaborationApplication(u, r.ID, newID, "switching", "completed"); err != nil {
		t.Fatal(err)
	}
	if err = s.AbandonCollaborationApplication(u, r.ID, strings.Repeat("e", 32), snapshot); err == nil {
		t.Fatal("abandon hid completed application")
	}
	if _, _, err = s.BeginCollaborationApplication(u, r.ID, strings.Repeat("e", 32), snapshot, r.Revision); err == nil {
		t.Fatal("duplicate successful application")
	}
}
