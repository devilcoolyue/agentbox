package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func collaborationFixture(t *testing.T) (*Store, User, CollaborationCreateInput, CollaborationConfig) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	u := User{Name: "alice", Role: RoleUser, CreatedAt: time.Now().UTC()}
	if err = s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if err = s.Put(Session{ID: "source", User: u.Name, Agent: "claude", AccountID: "main", Status: StatusStopped, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	roles := CollaborationRoles{Coordinator: CollaborationBinding{AgentType: "codex", AccountID: "main", Model: "planner"}, Executor: CollaborationBinding{AgentType: "claude", AccountID: "worker", Model: "builder"}}
	cfg := CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	in := CollaborationCreateInput{Scope: "fixture-scope", SourceSessionID: "source", Repository: ".", Baseline: strings.Repeat("a", 40), Text: "Implement synthetic feature", Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	return s, u, in, cfg
}

func TestCollaborationCreateReplayIdentityAndAtomicity(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	id := strings.Repeat("a", 32)
	var fresh atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, f, err := s.CreateCollaboration(u, id, in, cfg)
			if err != nil {
				t.Error(err)
			}
			if f {
				fresh.Add(1)
			}
		})
	}
	wg.Wait()
	if fresh.Load() != 1 {
		t.Fatal("multiple scheduling grants", fresh.Load())
	}
	r, err := s.CollaborationByRequest(u, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.CollaborationConfig(r.ID, 1)
	if err != nil || got != cfg {
		t.Fatal(got, err)
	}
	changed := in
	changed.Text = "different"
	if _, _, err = s.CreateCollaboration(u, id, changed, cfg); !errors.Is(err, ErrChatConflict) {
		t.Fatal(err)
	}
	foreign := u
	foreign.Name = "bob"
	if _, err = s.Collaboration(foreign, r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign read", err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER fail_collaboration_config BEFORE INSERT ON collaboration_configs BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateCollaboration(u, strings.Repeat("b", 32), in, cfg); err == nil {
		t.Fatal("expected failure")
	}
	if _, err = s.CollaborationByRequest(u, strings.Repeat("b", 32)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("partial create survived", err)
	}
	if err = s.DeleteUser(u.Name); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collaboration(u, r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleted identity retained access", err)
	}
	u.CreatedAt = u.CreatedAt.Add(time.Hour)
	if err = s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collaboration(u, r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("new identity inherited task", err)
	}
}

func TestCollaborationParticipantPublicationAndRestartFence(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	r, _, err := s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := Session{ID: "planner", User: u.Name, Agent: "codex", AccountID: "main", DefaultModel: "planner", Status: StatusStopped, CreatedAt: time.Now()}
	executor := Session{ID: "builder", User: u.Name, Agent: "claude", AccountID: "worker", DefaultModel: "builder", Status: StatusStopped, CreatedAt: time.Now()}
	r, st, err := s.PublishCollaborationParticipants(u, r.ID, r.Revision, coordinator, executor, CollaborationStepInput{Text: "Plan synthetic work", SnapshotID: strings.Repeat("b", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if managed, err := s.IsCollaborationSession(coordinator.ID); err != nil || !managed {
		t.Fatal(managed, err)
	}
	claimed, err := s.ClaimCollaborationStep(u, r.ID, st.ID, st.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimCollaborationStep(u, r.ID, st.ID, st.Revision); !errors.Is(err, ErrChatConflict) {
		t.Fatal("duplicate claim", err)
	}
	claimed, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, claimed.Revision, "running")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Collaboration(u, r.ID)
	if err != nil || recovered.State != CollabUncertain {
		t.Fatal(recovered, err)
	}
	if _, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, claimed.Revision, "finalizing"); !errors.Is(err, ErrChatConflict) {
		t.Fatal("old executor escaped restart fence", err)
	}
	r2, _, err := s.CreateCollaboration(u, strings.Repeat("c", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	r2, err = s.Collaboration(u, r2.ID)
	if err != nil || r2.State != CollabPaused {
		t.Fatal("accepted task resumed automatically", r2, err)
	}
}

func TestCollaborationActionsAreAtomicAndReplayBeforeRevision(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	r, _, err := s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := CollaborationActionInput{Scope: in.Scope, ExpectedRevision: r.Revision, Action: "pause"}
	id := strings.Repeat("b", 32)
	receipt, fresh, err := s.ApplyCollaborationAction(u, r.ID, id, a)
	if err != nil || !fresh {
		t.Fatal(receipt, fresh, err)
	}
	replay, fresh, err := s.ApplyCollaborationAction(u, r.ID, id, a)
	if err != nil || fresh || string(replay.Result) != string(receipt.Result) {
		t.Fatal(replay, fresh, err)
	}
	changed := a
	changed.Action = "cancel"
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, id, changed); !errors.Is(err, ErrChatConflict) {
		t.Fatal(err)
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil || r.State != CollabPaused {
		t.Fatal(r, err)
	}
	if err = s.AbandonCollaborationAction(u, r.ID, strings.Repeat("c", 32)); err != nil {
		t.Fatal(err)
	}
	a.ExpectedRevision = r.Revision
	a.Action = "cancel"
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, strings.Repeat("c", 32), a); !errors.Is(err, ErrChatGone) {
		t.Fatal("late action escaped fence", err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER fail_action BEFORE INSERT ON collaboration_actions BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, strings.Repeat("d", 32), a); err == nil {
		t.Fatal("expected receipt failure")
	}
	current, err := s.Collaboration(u, r.ID)
	if err != nil || current.State != r.State || current.Revision != r.Revision {
		t.Fatal("state changed without receipt", current, err)
	}
}

func TestCollaborationHandoffAndNextStepCommitTogether(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	in.AutoExecute = true
	cfg.AutoExecute = true
	r, _, err := s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	main := Session{ID: "planner", User: u.Name, Agent: "codex", AccountID: "main", DefaultModel: "planner", Status: StatusStopped, CreatedAt: time.Now()}
	worker := Session{ID: "builder", User: u.Name, Agent: "claude", AccountID: "worker", DefaultModel: "builder", Status: StatusStopped, CreatedAt: time.Now()}
	r, step, err := s.PublishCollaborationParticipants(u, r.ID, r.Revision, main, worker, CollaborationStepInput{Text: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	step, err = s.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "finalizing"} {
		step, err = s.AdvanceCollaborationStep(u, r.ID, step.ID, step.Revision, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	handoff := []byte(`{"version":1,"type":"plan","payload":{"summary":"Build feature","items":[{"id":"item-1","text":"Implement feature","acceptance":["test succeeds"]}]}}`)
	completion := CollaborationCompletion{Final: handoff, HistorySynced: true, UsageSettled: true, ProcessStopped: true}
	if _, err = s.db.Exec("CREATE TRIGGER fail_next_step BEFORE INSERT ON collaboration_steps WHEN NEW.kind='execute' BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteCollaborationStep(u, r.ID, step.ID, step.Revision, completion); err == nil {
		t.Fatal("expected queue failure")
	}
	prior, err := s.CollaborationStep(step.ID)
	if err != nil || prior.State != "finalizing" {
		t.Fatal("partial finalization", prior, err)
	}
	messages, err := s.CollaborationMessages(u, r.ID, 0)
	if err != nil || len(messages) != 0 {
		t.Fatal("partial handoff", messages, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_next_step"); err != nil {
		t.Fatal(err)
	}
	r, err = s.CompleteCollaborationStep(u, r.ID, step.ID, step.Revision, completion)
	if err != nil || r.State != CollabExecuting || r.PlanVersion != 1 {
		t.Fatal(r, err)
	}
	if _, err = s.CompleteCollaborationStep(u, r.ID, step.ID, step.Revision, completion); !errors.Is(err, ErrChatConflict) {
		t.Fatal("replayed handoff advanced", err)
	}
	messages, err = s.CollaborationMessages(u, r.ID, 0)
	if err != nil || len(messages) != 1 || messages[0].SenderRole != "coordinator" {
		t.Fatal(messages, err)
	}
	var count int
	var raw string
	if err = s.db.QueryRow("SELECT count(*),input_json FROM collaboration_steps WHERE run_id=? AND kind='execute'", r.ID).Scan(&count, &raw); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var input CollaborationStepInput
	if err = json.Unmarshal([]byte(raw), &input); err != nil || !strings.Contains(input.Text, "Build feature") || !strings.Contains(input.Text, "executor") {
		t.Fatal(input, err)
	}
}
