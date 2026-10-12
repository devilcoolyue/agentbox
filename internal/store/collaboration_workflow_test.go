package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/safefs"
)

func preparedWorkflow(t *testing.T, mode, command string) (*Store, User, CollaborationRun) {
	t.Helper()
	s, u, in, cfg := collaborationFixture(t)
	in.Workflow = &CollaborationWorkflow{Version: 1, Mode: mode}
	cfg.Workflow = &CollaborationWorkflow{Version: 1, Mode: mode}
	in.AutoExecute = true
	cfg.AutoExecute = true
	in.VerificationCommand = command
	cfg.VerificationCommand = command
	r, _, err := s.CreateCollaboration(u, NewCollaborationID(), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	baseline := collaboration.Snapshot{ID: NewCollaborationID(), ManifestSHA256: strings.Repeat("a", 64), Manifest: collaboration.SnapshotManifest{Version: 1}}
	if _, err = s.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, baseline); err != nil {
		t.Fatal(err)
	}
	main := Session{ID: "planner", User: u.Name, Agent: "codex", AccountID: "main", DefaultModel: "planner", Status: StatusStopped, CreatedAt: time.Now()}
	if mode == "direct" {
		main = Session{}
	}
	worker := Session{ID: "builder", User: u.Name, Agent: "claude", AccountID: "worker", DefaultModel: "builder", Status: StatusStopped, CreatedAt: time.Now()}
	r, _, err = s.PublishCollaborationParticipants(u, r.ID, r.Revision, main, worker, CollaborationStepInput{SnapshotID: baseline.ID})
	if err != nil {
		t.Fatal(err)
	}
	return s, u, r
}

func finishWorkflow(t *testing.T, s *Store, u User, r CollaborationRun, kind string, payload any) CollaborationRun {
	t.Helper()
	st := activeCollaborationStep(t, s, r.ID)
	st, err := s.ClaimCollaborationStep(u, r.ID, st.ID, st.Revision)
	if err != nil {
		t.Fatal(err)
	}
	st, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, st.Revision, "running")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCollaborationProviderSession(u, r.ID, st.ID, st.Revision, st.Role+"-native"); err != nil {
		t.Fatal(err)
	}
	st, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, st.Revision, "finalizing")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot string
	if kind == "result" || (kind == "question" && st.Role == "executor") {
		r, err = s.Collaboration(u, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		snap := collaboration.Snapshot{ID: NewCollaborationID(), ManifestSHA256: strings.Repeat("b", 64), Manifest: collaboration.SnapshotManifest{Version: 1}}
		if _, err = s.RegisterCollaborationSnapshot(u, r.ID, r.Revision, st.ID, st.Revision, snap); err != nil {
			t.Fatal(err)
		}
		snapshot = snap.ID
	}
	body, _ := json.Marshal(payload)
	raw, _ := json.Marshal(collaboration.Handoff{Version: 1, Type: kind, Payload: body})
	r, err = s.CompleteCollaborationStep(u, r.ID, st.ID, st.Revision, CollaborationCompletion{Final: raw, SnapshotID: snapshot, HistorySynced: true, UsageSettled: true, ProcessStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func verifyWorkflow(t *testing.T, s *Store, u User, r CollaborationRun, state string) CollaborationRun {
	t.Helper()
	v, fresh, err := s.BeginCollaborationVerification(u, r.ID, r.Revision)
	if err != nil || !fresh {
		t.Fatal(v, fresh, err)
	}
	if v.State == "running" {
		if err = s.FinishCollaborationVerification(u, v, state, "test evidence", time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	r, err = s.QueueCollaborationReview(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCollaborationWorkflowDirectUsesOneParticipantAndDeliveryGates(t *testing.T) {
	for _, command := range []string{"", "npm test"} {
		t.Run(command, func(t *testing.T) {
			s, u, r := preparedWorkflow(t, "direct", command)
			ps, err := s.CollaborationParticipants(u, r.ID)
			if err != nil || len(ps) != 1 || ps[0].Role != "executor" {
				t.Fatal(ps, err)
			}
			step := activeCollaborationStep(t, s, r.ID)
			if step.Kind != "execute" || r.PlanVersion != 0 {
				t.Fatal(step, r)
			}
			w, err := s.CollaborationWork(u, r.ID)
			if err != nil || w.Input.Context == nil || !w.Input.Context.Rebuilt {
				t.Fatal(w, err)
			}
			r = finishWorkflow(t, s, u, r, "result", collaboration.Result{Summary: "Implemented", Completed: []string{"feature"}})
			if r.WaitReason != "verification_required" {
				t.Fatal(r)
			}
			snap, err := s.CollaborationLatestArtifact(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.DeliverCollaboration(u, r.ID, r.Revision, snap.ID); err == nil {
				t.Fatal("bypassed verification")
			}
			oldRevision := r.Revision
			r = verifyWorkflow(t, s, u, r, "completed")
			if r.WaitReason != "delivery_required" {
				t.Fatal(r)
			}
			if _, err = s.QueueCollaborationReview(u, r.ID, oldRevision); !errors.Is(err, ErrChatConflict) {
				t.Fatal("duplicate scheduling", err)
			}
			r, err = s.DeliverCollaboration(u, r.ID, r.Revision, snap.ID)
			if err != nil || r.State != CollabCompleted {
				t.Fatal(r, err)
			}
			if command == "" && r.WaitReason != "verification_not_requested" {
				t.Fatal("claimed independent review", r)
			}
			var count int
			if err = s.db.QueryRow("SELECT COUNT(*) FROM collaboration_steps WHERE run_id=?", r.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestCollaborationWorkflowDirectRepairsFailedVerificationAndRemaining(t *testing.T) {
	for _, scenario := range []string{"failed", "remaining", "limit"} {
		t.Run(scenario, func(t *testing.T) {
			s, u, r := preparedWorkflow(t, "direct", "npm test")
			result := collaboration.Result{Summary: "Attempt", Completed: []string{"code"}}
			if scenario == "remaining" {
				result.Remaining = []string{"still needs docs"}
			}
			if scenario == "limit" {
				if _, err := s.db.Exec("UPDATE collaborations SET max_repairs=0 WHERE id=?", r.ID); err != nil {
					t.Fatal(err)
				}
			}
			r = finishWorkflow(t, s, u, r, "result", result)
			state := "failed"
			if scenario == "remaining" {
				state = "completed"
			}
			r = verifyWorkflow(t, s, u, r, state)
			if scenario == "limit" {
				if r.State != CollabPaused || r.WaitReason != "repair_limit" {
					t.Fatal(r)
				}
				return
			}
			if r.State != CollabExecuting || activeCollaborationStep(t, s, r.ID).Kind != "repair" {
				t.Fatal(r)
			}
			w, err := s.CollaborationWork(u, r.ID)
			if err != nil || !strings.Contains(w.Input.Text, "platform_verification") {
				t.Fatal(w, err)
			}
			r = finishWorkflow(t, s, u, r, "result", collaboration.Result{Summary: "Fixed", Completed: []string{"feature and tests"}})
			r = verifyWorkflow(t, s, u, r, "completed")
			if r.WaitReason != "delivery_required" {
				t.Fatal(r)
			}
		})
	}
}

func TestCollaborationWorkflowDirectQuestionsResumeExecutorAndUseIncrementalContext(t *testing.T) {
	s, u, r := preparedWorkflow(t, "direct", "")
	r = finishWorkflow(t, s, u, r, "question", collaboration.Question{Text: "First question?", Blocking: true})
	if r.State != CollabWaitingUser {
		t.Fatal(r)
	}
	messages, err := s.CollaborationMessages(u, r.ID, 0)
	if err != nil || messages[0].RecipientRole != "user" {
		t.Fatal(messages, err)
	}
	r = applyCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: messages[0].ID, Text: "first answer constraint"})
	w, err := s.CollaborationWork(u, r.ID)
	if err != nil || w.Step.Role != "executor" || !strings.Contains(w.Input.Text, "first answer constraint") || strings.Contains(w.Input.Text, "First question?") {
		t.Fatal(w, err)
	}
	if w.Input.Context.Rebuilt || w.Input.Context.Through != 2 {
		t.Fatal(w.Input.Context)
	}
	// No acknowledgement until the successful, settled finalization of this turn.
	var cursor int64
	if err = s.db.QueryRow("SELECT context_seq FROM collaboration_participants WHERE run_id=?", r.ID).Scan(&cursor); err != nil || cursor != 0 {
		t.Fatal(cursor, err)
	}
	r = finishWorkflow(t, s, u, r, "question", collaboration.Question{Text: "Second question?", Blocking: true})
	if err = s.db.QueryRow("SELECT context_seq FROM collaboration_participants WHERE run_id=?", r.ID).Scan(&cursor); err != nil || cursor != 2 {
		t.Fatal(cursor, err)
	}
	messages, err = s.CollaborationMessages(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	r = applyCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: messages[len(messages)-1].ID, Text: "second answer constraint"})
	w, err = s.CollaborationWork(u, r.ID)
	if err != nil || strings.Contains(w.Input.Text, "first answer constraint") || !strings.Contains(w.Input.Text, "second answer constraint") {
		t.Fatal(w, err)
	}
	st, err := s.ClaimCollaborationStep(u, r.ID, w.Step.ID, w.Step.Revision)
	if err != nil {
		t.Fatal(err)
	}
	st, err = s.AdvanceCollaborationStep(u, r.ID, st.ID, st.Revision, "running")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil || r.State != CollabUncertain {
		t.Fatal(r, err)
	}
	if err = s.db.QueryRow("SELECT context_seq FROM collaboration_participants WHERE run_id=?", r.ID).Scan(&cursor); err != nil || cursor != 2 {
		t.Fatal("uncertain context acknowledged", cursor, err)
	}
}

func TestCollaborationWorkflowFrozenAndLegacyReplay(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	in.Workflow = &CollaborationWorkflow{Version: 1, Mode: "direct"}
	cfg.Workflow = &CollaborationWorkflow{Version: 1, Mode: "direct"}
	id := NewCollaborationID()
	r, _, err := s.CreateCollaboration(u, id, in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, fresh, err := s.CreateCollaboration(u, id, in, cfg)
	if err != nil || fresh {
		t.Fatal(fresh, err)
	}
	changed := in
	changed.Workflow = nil
	legacy := cfg
	legacy.Workflow = nil
	if _, _, err = s.CreateCollaboration(u, id, changed, legacy); !errors.Is(err, ErrChatConflict) {
		t.Fatal("workflow omitted on replay", err)
	}
	cfg.Workflow = &CollaborationWorkflow{Version: 1, Mode: "reviewed"}
	raw, _ := json.Marshal(cfg)
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "configure", Payload: raw}); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("workflow changed", err)
	}
	in.Workflow = &CollaborationWorkflow{Version: 2, Mode: "direct"}
	cfg.Workflow = in.Workflow
	if _, _, err = s.CreateCollaboration(u, NewCollaborationID(), in, cfg); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("unknown workflow version", err)
	}
}

func TestCollaborationWorkflowContextCompactionPreservesHardInstructions(t *testing.T) {
	s, _, r := preparedWorkflow(t, "direct", "")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	appendMessage := func(sender, kind string, body any) {
		raw, _ := json.Marshal(body)
		if e := appendCollaborationMessageTx(tx, &r, NewCollaborationID(), "", sender, "executor", kind, "", raw); e != nil {
			t.Fatal(e)
		}
	}
	appendMessage("user", "system", map[string]string{"text": "NEVER remove production data"})
	appendMessage("coordinator", "plan", collaboration.Plan{Summary: "Plan", Constraints: []string{"Keep API compatibility"}})
	appendMessage("coordinator", "plan", collaboration.Plan{Summary: "Code map plan", CodeMap: []collaboration.CodeMapEntry{{Path: "billing.go", Purpose: "billing", Constraints: []string{"Do not round integer micro USD"}}}})
	for range 12 {
		appendMessage("executor", "result", collaboration.Result{Summary: strings.Repeat("old evidence ", 1000)})
	}
	messages, boundary, err := collaborationContextTx(tx, &r, "executor", 1, 32000)
	if err != nil || boundary.Omitted == 0 || !boundary.Rebuilt {
		t.Fatal(boundary, err)
	}
	raw, _ := json.Marshal(messages)
	if !strings.Contains(string(raw), "NEVER remove production data") || !strings.Contains(string(raw), "Keep API compatibility") || !strings.Contains(string(raw), "Do not round integer micro USD") || !strings.Contains(string(raw), "omitted_messages") {
		t.Fatal("required context lost")
	}
	appendMessage("user", "system", map[string]string{"text": strings.Repeat("hard user constraint", 2000)})
	if _, _, err = collaborationContextTx(tx, &r, "executor", 1, 32000); !errors.Is(err, errCollaborationContextLimit) {
		t.Fatal("hard constraints silently dropped", err)
	}
}

func TestCollaborationWorkflowMigrationPreservesLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(db, migrations()[:15]); err != nil {
		t.Fatal(err)
	}
	// Existing provider identity survives with an unacknowledged context boundary.
	if _, err = db.Exec(`INSERT INTO collaboration_participants(run_id,role,generation,config_version,session_id,provider_session,created_at) VALUES('legacy','executor',1,1,'legacy-worker','native-thread','fixture')`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	var provider, ack string
	var cursor int64
	if err = db.QueryRow("SELECT provider_session,context_provider,context_seq FROM collaboration_participants").Scan(&provider, &ack, &cursor); err != nil || provider != "native-thread" || ack != "" || cursor != 0 {
		t.Fatal(provider, ack, cursor, err)
	}
	db.Close()
}

func TestCollaborationWorkflowReviewEvidenceFrozenAndReused(t *testing.T) {
	s, u, r := preparedWorkflow(t, "reviewed", "")
	r = finishWorkflow(t, s, u, r, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "item", Text: "Implement", Acceptance: []string{"tests"}}}})
	r = finishWorkflow(t, s, u, r, "result", collaboration.Result{Summary: "Implemented"})
	if _, _, err := s.BeginCollaborationVerification(u, r.ID, r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueCollaborationReview(u, r.ID, r.Revision); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("new reviewed task lacked evidence", err)
	}
	var baseline CollaborationArtifact
	baseline, err := scanCollaborationArtifact(s.db.QueryRow("SELECT "+collaborationArtifactCols+" FROM collaboration_artifacts WHERE run_id=? AND kind='baseline'", r.ID))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.CollaborationLatestArtifact(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	evidence, err := collaboration.BuildReviewEvidence(t.Context(), root, collaboration.Snapshot{ID: baseline.ID, ManifestSHA256: baseline.SHA256}, collaboration.Snapshot{ID: snapshot.ID, ManifestSHA256: snapshot.SHA256}, r.PlanVersion)
	if err != nil {
		t.Fatal(err)
	}
	wrong := evidence
	wrong.SnapshotSHA256 = strings.Repeat("c", 64)
	if _, err = s.QueueCollaborationReview(u, r.ID, r.Revision, &wrong); !errors.Is(err, ErrChatConflict) {
		t.Fatal("unrelated snapshot digest", err)
	}
	var rows int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM collaboration_review_evidence WHERE run_id=?", r.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("partial evidence frozen", rows, err)
	}
	r, err = s.QueueCollaborationReview(u, r.ID, r.Revision, &evidence)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.CollaborationWork(u, r.ID)
	if err != nil || !strings.Contains(w.Input.Text, "review_evidence") || !strings.Contains(w.Input.Text, evidence.ChangesSHA256) {
		t.Fatal(w, err)
	}
	r = finishWorkflow(t, s, u, r, "question", collaboration.Question{Text: "Should this behavior change?", Blocking: true})
	msgs, err := s.CollaborationMessages(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	r = applyCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: msgs[len(msgs)-1].ID, Text: "Keep the new behavior"})
	w, err = s.CollaborationWork(u, r.ID)
	if err != nil || w.Step.Kind != "review" || !strings.Contains(w.Input.Text, evidence.ChangesSHA256) {
		t.Fatal("follow-up lost frozen evidence", w, err)
	}
	// A deleted owner must not leave source diff material in the evidence table.
	if err = s.DeleteUser(u.Name); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM collaboration_review_evidence WHERE run_id=?", r.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("owner deletion retained diff", rows, err)
	}
}

func TestCollaborationWorkflowContextRejectsChangedNativeThread(t *testing.T) {
	s, u, r := preparedWorkflow(t, "direct", "")
	r = finishWorkflow(t, s, u, r, "question", collaboration.Question{Text: "Choose behavior", Blocking: true})
	msgs, err := s.CollaborationMessages(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	r = applyCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: msgs[0].ID, Text: "Keep current behavior"})
	if _, err = s.db.Exec("UPDATE sessions SET chat_session='different-native' WHERE id='builder'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CollaborationWork(u, r.ID); !errors.Is(err, ErrChatConflict) {
		t.Fatal("delta escaped native-thread binding", err)
	}
}

func TestCollaborationWorkflowDirectMalformedRepairCanReformatWithoutRerunning(t *testing.T) {
	s, u, r := preparedWorkflow(t, "direct", "npm test")
	r = finishWorkflow(t, s, u, r, "result", collaboration.Result{Summary: "Attempt"})
	r = verifyWorkflow(t, s, u, r, "failed")
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
	r, err = s.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := collaboration.Snapshot{ID: NewCollaborationID(), ManifestSHA256: strings.Repeat("c", 64), Manifest: collaboration.SnapshotManifest{Version: 1}}
	if _, err = s.RegisterCollaborationSnapshot(u, r.ID, r.Revision, step.ID, step.Revision, snapshot); err != nil {
		t.Fatal(err)
	}
	r, err = s.CompleteCollaborationStep(u, r.ID, step.ID, step.Revision, CollaborationCompletion{Final: []byte("I repaired it"), SnapshotID: snapshot.ID, HistorySynced: true, UsageSettled: true, ProcessStopped: true})
	if err != nil || r.WaitReason != "handoff_invalid" {
		t.Fatal(r, err)
	}
	r = applyCollaborationFixture(t, s, u, r, "repair_handoff", nil)
	w, err := s.CollaborationWork(u, r.ID)
	if err != nil || w.Step.Kind != "handoff_repair" || w.Input.RepairKind != "repair" || w.Input.SnapshotID != snapshot.ID || !strings.Contains(w.Input.Text, "Do not repeat implementation") {
		t.Fatal(w, err)
	}
	var n int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM collaboration_verifications WHERE run_id=? AND snapshot_id=?", r.ID, snapshot.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal("reformat launched tests", n, err)
	}
}

func TestCollaborationWorkflowNewGenerationRebuildsRequiredContext(t *testing.T) {
	s, u, r := preparedWorkflow(t, "direct", "")
	r = finishWorkflow(t, s, u, r, "question", collaboration.Question{Text: "Which behavior?", Blocking: true})
	msgs, err := s.CollaborationMessages(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	r = applyCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: msgs[0].ID, Text: "Preserve cents exactly"})
	r = finishWorkflow(t, s, u, r, "question", collaboration.Question{Text: "Any other condition?", Blocking: true})
	cfg, err := s.CollaborationConfig(r.ID, r.ConfigVersion)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxSteps++
	r = applyCollaborationFixture(t, s, u, r, "configure", cfg)
	r, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	worker := Session{ID: "builder-generation-2", User: u.Name, Agent: "claude", AccountID: "worker", DefaultModel: "builder", Status: StatusStopped, CreatedAt: time.Now()}
	r, _, err = s.PublishCollaborationParticipants(u, r.ID, r.Revision, Session{}, worker, CollaborationStepInput{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.CollaborationWork(u, r.ID)
	if err != nil || w.Step.ParticipantGeneration != 2 || !w.Input.Context.Rebuilt || w.Input.Context.After != 0 || !strings.Contains(w.Input.Text, "Preserve cents exactly") {
		t.Fatal(w, err)
	}
}

func TestCollaborationWorkflowRepairQuestionKeepsPreviousVerificationAttribution(t *testing.T) {
	s, u, r := preparedWorkflow(t, "direct", "npm test")
	r = finishWorkflow(t, s, u, r, "result", collaboration.Result{Summary: "First attempt"})
	failedSnapshot, err := s.CollaborationLatestArtifact(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = verifyWorkflow(t, s, u, r, "failed")
	r = finishWorkflow(t, s, u, r, "question", collaboration.Question{Text: "Which failing case should be preserved?", Blocking: true})
	questionSnapshot, err := s.CollaborationLatestArtifact(u, r.ID)
	if err != nil || questionSnapshot.ID == failedSnapshot.ID {
		t.Fatal(questionSnapshot, err)
	}
	msgs, err := s.CollaborationMessages(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	r = applyCollaborationFixture(t, s, u, r, "answer", collaboration.Answer{QuestionID: msgs[len(msgs)-1].ID, Text: "Preserve the decimal case"})
	w, err := s.CollaborationWork(u, r.ID)
	if err != nil || w.Step.Kind != "repair" || w.Input.SnapshotID != questionSnapshot.ID || !strings.Contains(w.Input.Text, "previous_platform_verification") || !strings.Contains(w.Input.Text, failedSnapshot.ID) {
		t.Fatal(w, err)
	}
	r = finishWorkflow(t, s, u, r, "result", collaboration.Result{Summary: "Repaired"})
	r = verifyWorkflow(t, s, u, r, "completed")
	if r.WaitReason != "delivery_required" {
		t.Fatal(r)
	}
}

func TestCollaborationWorkflowDirectReconfigurationDoesNotBypassRepairCap(t *testing.T) {
	for _, limit := range []string{"repair_limit", "step_limit"} {
		t.Run(limit, func(t *testing.T) {
			s, u, r := preparedWorkflow(t, "direct", "npm test")
			column := "max_repairs=0"
			if limit == "step_limit" {
				column = "max_steps=1"
			}
			if _, err := s.db.Exec("UPDATE collaborations SET "+column+" WHERE id=?", r.ID); err != nil {
				t.Fatal(err)
			}
			r = finishWorkflow(t, s, u, r, "result", collaboration.Result{Summary: "Incomplete"})
			r = verifyWorkflow(t, s, u, r, "failed")
			if r.WaitReason != limit {
				t.Fatal(r)
			}
			cfg, err := s.CollaborationConfig(r.ID, r.ConfigVersion)
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxSteps++
			cfg.MaxRepairs = 0
			raw, _ := json.Marshal(cfg)
			if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "configure", Payload: raw}); !errors.Is(err, collaboration.ErrState) {
				t.Fatal("repair cap bypassed", err)
			}
			cfg.MaxRepairs = 1
			r = applyCollaborationFixture(t, s, u, r, "configure", cfg)
			var count int
			if err = s.db.QueryRow("SELECT repairs_used FROM collaborations WHERE id=?", r.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("new executor generation was uncounted", count, err)
			}
			if r.State != CollabAccepted {
				t.Fatal(r)
			}
		})
	}
}

func TestCollaborationWorkflowContextCountsRawJSONEscaping(t *testing.T) {
	for _, kind := range []string{"answer", "plan"} {
		t.Run(kind, func(t *testing.T) {
			s, _, r := preparedWorkflow(t, "direct", "")
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// Do not marshal the fixture: real provider and HTTP JSON may contain
			// these literal characters, which are escaped only in the final prompt.
			body := json.RawMessage(`{"question_id":"` + NewCollaborationID() + `","text":"` + strings.Repeat("<&>", 5400) + `"}`)
			if kind == "plan" {
				body = json.RawMessage(`{"summary":"plan","items":[{"id":"one","text":"Implement","acceptance":["tests"]}],"constraints":["` + strings.Repeat("<&>", 2700) + `","` + strings.Repeat("<&>", 2700) + `"]}`)
			}
			if _, err = collaboration.ParseHandoff([]byte(`{"version":1,"type":"`+kind+`","payload":`+string(body)+`}`), collaboration.Coordinator); err != nil {
				t.Fatal("fixture is not a valid raw handoff", err)
			}
			for range 12 {
				if err = appendCollaborationMessageTx(tx, &r, NewCollaborationID(), "", "coordinator", "executor", kind, "", body); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err = collaborationContextTx(tx, &r, "executor", 1, 512<<10); !errors.Is(err, errCollaborationContextLimit) {
				t.Fatal("escaped hard constraints exceeded actual prompt budget", err)
			}
			// Queuing pauses at the durable boundary instead of failing the whole
			// preceding completion or creating an unbounded new model input.
			r.State = CollabExecuting
			st, err := queueCollaborationStepTx(tx, &r, "execute", "")
			if err != nil || st.ID != "" || r.State != CollabPaused || r.WaitReason != "context_limit" || r.NextKind != "execute" {
				t.Fatal(st, r, err)
			}
		})
	}
}
