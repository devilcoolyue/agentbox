package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agentbox/internal/collaboration"
)

func preparedCollaboration(t *testing.T, auto bool) (*Store, User, CollaborationRun) {
	t.Helper()
	s, u, in, cfg := collaborationFixture(t)
	in.AutoExecute = auto
	cfg.AutoExecute = auto
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
	r, _, err = s.PublishCollaborationParticipants(u, r.ID, r.Revision, main, worker, CollaborationStepInput{Text: "Plan"})
	if err != nil {
		t.Fatal(err)
	}
	return s, u, r
}

func activeCollaborationStep(t *testing.T, s *Store, run string) CollaborationStep {
	t.Helper()
	st, err := scanCollaborationStep(s.db.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE run_id=? AND state='accepted'", run))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func finishCollaborationFixture(t *testing.T, s *Store, u User, r CollaborationRun, kind string, payload any) CollaborationRun {
	t.Helper()
	st := activeCollaborationStep(t, s, r.ID)
	st, err := s.ClaimCollaborationStep(u, r.ID, st.ID, st.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "finalizing"} {
		st, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, st.Revision, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	body, _ := json.Marshal(payload)
	raw, _ := json.Marshal(collaboration.Handoff{Version: 1, Type: kind, Payload: body})
	r, err = s.CompleteCollaborationStep(u, r.ID, st.ID, st.Revision, CollaborationCompletion{Final: raw, HistorySynced: true, UsageSettled: true, ProcessStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func applyCollaborationFixture(t *testing.T, s *Store, u User, r CollaborationRun, action string, payload any) CollaborationRun {
	t.Helper()
	var raw []byte
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	_, fresh, err := s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Request.Scope, ExpectedRevision: r.Revision, Action: action, Payload: raw})
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCollaborationEscalatedQuestionReturnsToOriginalWorker(t *testing.T) {
	s, u, r := preparedCollaboration(t, true)
	r = finishCollaborationFixture(t, s, u, r, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "item", Text: "Implement", Acceptance: []string{"test"}}}})
	r = finishCollaborationFixture(t, s, u, r, "question", collaboration.Question{Text: "Which duplicate policy?", Blocking: true})
	messages, err := s.CollaborationMessages(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	workerQuestion := messages[len(messages)-1].ID
	r = finishCollaborationFixture(t, s, u, r, "question", collaboration.Question{Text: "User, should duplicates be replaced?", Blocking: true})
	messages, err = s.CollaborationMessages(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	userQuestion := messages[len(messages)-1].ID
	if r.State != CollabWaitingUser {
		t.Fatal(r)
	}
	bad, _ := json.Marshal(collaboration.Answer{QuestionID: workerQuestion, Text: "stale"})
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Request.Scope, ExpectedRevision: r.Revision, Action: "answer", Payload: bad}); !errors.Is(err, ErrChatConflict) {
		t.Fatal("wrong question accepted", err)
	}
	r = applyCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: userQuestion, Text: "Skip existing accounts"})
	st := activeCollaborationStep(t, s, r.ID)
	if st.Kind != "clarify" || r.State != CollabWaitingAnswer {
		t.Fatal(st, r)
	}
	var input string
	if err = s.db.QueryRow("SELECT input_json FROM collaboration_steps WHERE id=?", st.ID).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, workerQuestion) || !strings.Contains(input, "Skip existing accounts") {
		t.Fatal("answer or original question missing from next input")
	}
	r = finishCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: workerQuestion, Text: "Skip existing accounts"})
	st = activeCollaborationStep(t, s, r.ID)
	if st.Kind != "execute" || st.Role != "executor" || r.State != CollabExecuting {
		t.Fatal(st, r)
	}
	var open int
	if err = s.db.QueryRow("SELECT count(*) FROM collaboration_questions WHERE run_id=? AND state='open'", r.ID).Scan(&open); err != nil || open != 0 {
		t.Fatal(open, err)
	}
}

func TestCollaborationPauseBoundaryAndResumeQueue(t *testing.T) {
	s, u, r := preparedCollaboration(t, true)
	st := activeCollaborationStep(t, s, r.ID)
	st, err := s.ClaimCollaborationStep(u, r.ID, st.ID, st.Revision)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = applyCollaborationFixture(t, s, u, r, "pause", nil)
	for _, state := range []string{"running", "finalizing"} {
		st, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, st.Revision, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err = s.CompleteCollaborationStep(u, r.ID, st.ID, st.Revision, CollaborationCompletion{Final: []byte(`{"version":1,"type":"plan","payload":{"summary":"Plan","items":[{"id":"one","text":"Implement","acceptance":["test"]}]}}`), HistorySynced: true, UsageSettled: true, ProcessStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != CollabPaused || r.NextKind != "execute" || r.ResumeState != CollabExecuting {
		t.Fatal(r)
	}
	r = applyCollaborationFixture(t, s, u, r, "resume", nil)
	if r.State != CollabExecuting || r.NextKind != "" || activeCollaborationStep(t, s, r.ID).Kind != "execute" {
		t.Fatal(r)
	}
}

func TestCollaborationReconfigureSwapsAllBindingsAndFencesOldGeneration(t *testing.T) {
	s, u, r := preparedCollaboration(t, false)
	oldStep := activeCollaborationStep(t, s, r.ID)
	r = applyCollaborationFixture(t, s, u, r, "pause", nil)
	cfg, err := s.CollaborationConfig(r.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Coordinator, cfg.Roles.Executor = cfg.Roles.Executor, cfg.Roles.Coordinator
	cfg.Roles.Executor.Model = "new-model"
	r = applyCollaborationFixture(t, s, u, r, "configure", cfg)
	if r.ConfigVersion != 2 || r.State != CollabAccepted {
		t.Fatal(r)
	}
	if _, err = s.ClaimCollaborationStep(u, r.ID, oldStep.ID, oldStep.Revision); err == nil {
		t.Fatal("old step retained execution rights")
	}
	prior, err := s.CollaborationConfig(r.ID, 1)
	if err != nil || prior.Roles.Coordinator.AgentType != "codex" {
		t.Fatal(prior, err)
	}
	r, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	main := Session{ID: "new-main", User: u.Name, Agent: cfg.Roles.Coordinator.AgentType, AccountID: cfg.Roles.Coordinator.AccountID, DefaultModel: cfg.Roles.Coordinator.Model, Status: StatusStopped, CreatedAt: time.Now()}
	worker := Session{ID: "new-worker", User: u.Name, Agent: cfg.Roles.Executor.AgentType, AccountID: cfg.Roles.Executor.AccountID, DefaultModel: cfg.Roles.Executor.Model, Status: StatusStopped, CreatedAt: time.Now()}
	r, st, err := s.PublishCollaborationParticipants(u, r.ID, r.Revision, main, worker, CollaborationStepInput{Text: "Review existing plan and work"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind != "plan" || st.ParticipantGeneration != 2 || st.ConfigVersion != 2 {
		t.Fatal(st)
	}
	participants, err := s.CollaborationParticipants(u, r.ID)
	if err != nil || len(participants) != 4 {
		t.Fatal(participants, err)
	}
	for _, p := range participants {
		if p.Generation == 1 && p.State != "revoked" {
			t.Fatal("old participant active", p)
		}
	}
}

func TestCollaborationStopEvidenceCannotAcknowledgeAnotherEpoch(t *testing.T) {
	s, u, r := preparedCollaboration(t, false)
	st := activeCollaborationStep(t, s, r.ID)
	before, err := s.CollaborationParticipants(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err = s.ClaimCollaborationStep(u, r.ID, st.ID, st.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmCollaborationStopped(u, r.ID, r.Revision, before); err == nil {
		t.Fatal("old epoch acknowledged new process")
	}
	after, err := s.CollaborationParticipants(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmCollaborationStopped(u, r.ID, r.Revision, after); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.CollaborationConfig(r.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Coordinator.Model = "replacement"
	raw, _ := json.Marshal(cfg)
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Request.Scope, ExpectedRevision: r.Revision, Action: "configure", Payload: raw}); err == nil {
		t.Fatal("stop proof alone erased uncertain result")
	}
	if _, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, st.Revision, "running"); err == nil {
		t.Fatal("stale writer survived recovery")
	}
}

func TestCollaborationCancelWaitsForStopAndUncertainReview(t *testing.T) {
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
	r, err = s.FailCollaborationStep(u, r.ID, step.ID, step.Revision, "execution_failed", false)
	if err != nil || r.State != CollabUncertain {
		t.Fatal(r, err)
	}
	step, err = s.CollaborationStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"step_id": step.ID, "step_revision": step.Revision, "acknowledged": true}
	raw, _ := json.Marshal(input)
	a := CollaborationActionInput{Scope: r.Request.Scope, ExpectedRevision: r.Revision, Action: "review", Payload: raw}
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), a); err == nil {
		t.Fatal("review released live process")
	}
	participants, err := s.CollaborationParticipants(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmCollaborationStopped(u, r.ID, r.Revision, participants); err != nil {
		t.Fatal(err)
	}
	r = applyCollaborationFixture(t, s, u, r, "review", input)
	if r.State != CollabPaused || r.WaitReason != "result_reviewed" {
		t.Fatal(r)
	}
	r = applyCollaborationFixture(t, s, u, r, "cancel", nil)
	r, err = s.FinalizeCollaborationCancellation(u, r.ID, r.Revision)
	if err != nil || r.State != CollabCancelled {
		t.Fatal(r, err)
	}
	old, err := s.CollaborationStep(step.ID)
	if err != nil || old.State != "reviewed" {
		t.Fatal("review rewritten as successful", old, err)
	}
}

func TestCollaborationStartupFailureRebuildsSameConfigurationOnce(t *testing.T) {
	s, u, r := preparedCollaboration(t, false)
	step := activeCollaborationStep(t, s, r.ID)
	var err error
	step, err = s.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.FailCollaborationStep(u, r.ID, step.ID, step.Revision, "execution_failed", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != CollabPaused {
		t.Fatal(r.State)
	}
	cfg, err := s.CollaborationConfig(r.ID, r.ConfigVersion)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	actionID := NewCollaborationID()
	input := CollaborationActionInput{Scope: r.Request.Scope, ExpectedRevision: r.Revision, Action: "configure", Payload: raw}
	_, fresh, err := s.ApplyCollaborationAction(u, r.ID, actionID, input)
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	rebuilt, err := s.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.State != CollabAccepted || rebuilt.ConfigVersion != r.ConfigVersion+1 || rebuilt.StepsUsed != r.StepsUsed || rebuilt.Request.Text != r.Request.Text {
		t.Fatalf("startup recovery did not rebuild while retaining history: %#v", rebuilt)
	}
	prior, err := s.CollaborationStep(step.ID)
	if err != nil || prior.State != "failed" {
		t.Fatal("failure evidence lost", prior, err)
	}
	participants, err := s.CollaborationParticipants(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range participants {
		if p.State != "revoked" {
			t.Fatal("retired participant still active")
		}
	}
	_, fresh, err = s.ApplyCollaborationAction(u, r.ID, actionID, input)
	if err != nil || fresh {
		t.Fatal("action replay repeated preparation", fresh, err)
	}
	replayed, err := s.Collaboration(u, r.ID)
	if err != nil || replayed.Revision != rebuilt.Revision || replayed.ConfigVersion != rebuilt.ConfigVersion {
		t.Fatal("replay changed generation", err)
	}
}
