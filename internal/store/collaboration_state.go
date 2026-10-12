package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"agentbox/internal/collaboration"
)

func collaborationRunTx(tx *sql.Tx, u User, id string) (CollaborationRun, error) {
	if err := checkCreationOwner(tx, u); err != nil {
		return CollaborationRun{}, err
	}
	return scanCollaboration(tx.QueryRow("SELECT "+collaborationCols+" FROM collaborations WHERE id=? AND user=? AND user_created_at=?", id, u.Name, ownerEpoch(u)))
}

func collaborationEventTx(tx *sql.Tx, run, kind string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO collaboration_events(run_id,seq,type,body_json,created_at) SELECT ?,COALESCE(MAX(seq),0)+1,?,?,? FROM collaboration_events WHERE run_id=?", run, kind, string(raw), usageTimestamp(time.Now()), run)
	return err
}

func saveCollaborationTx(tx *sql.Tx, r *CollaborationRun) error {
	old := r.Revision
	r.Revision++
	r.UpdatedAt = usageTimestamp(time.Now())
	res, err := tx.Exec("UPDATE collaborations SET state=?,phase=?,wait_reason=?,revision=?,config_version=?,plan_version=?,steps_used=?,updated_at=?,resume_state=?,next_kind=?,max_repairs=?,max_steps=?,budget_micro_usd=? WHERE id=? AND revision=?", r.State, r.Phase, r.WaitReason, r.Revision, r.ConfigVersion, r.PlanVersion, r.StepsUsed, r.UpdatedAt, r.ResumeState, r.NextKind, r.MaxRepairs, r.MaxSteps, r.BudgetMicroUSD, r.ID, old)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrChatConflict
	}
	// Events contain state, not the frozen input on every transition.
	view := *r
	view.Request = nil
	return collaborationEventTx(tx, r.ID, "state", view)
}

// BeginCollaborationPreparation grants a single preparation attempt. Restart
// recovery pauses it rather than replaying filesystem writes automatically.
func (s *Store) BeginCollaborationPreparation(u User, id string, revision int64) (CollaborationRun, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, id)
	if err != nil {
		return r, err
	}
	if r.Revision != revision || r.State != CollabAccepted {
		return r, ErrChatConflict
	}
	if err = collaborationDecisionReadyTx(tx, id); err != nil {
		return r, err
	}
	if paused, e := pauseForCollaborationBudgetTx(tx, &r); e != nil {
		return r, e
	} else if paused {
		if e = saveCollaborationTx(tx, &r); e != nil {
			return r, e
		}
		if e = tx.Commit(); e != nil {
			return r, e
		}
		return r, collaboration.ErrState
	}
	r.State = CollabPreparing
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

type CollaborationStepInput struct {
	Context    *CollaborationContextBoundary `json:"context_boundary,omitempty"`
	RepairKind string                        `json:"repair_kind,omitempty"`
	Text       string                        `json:"text"`
	SnapshotID string                        `json:"snapshot_id"`
	ReplyTo    string                        `json:"reply_to,omitempty"`
}

func addCollaborationStepTx(tx *sql.Tx, r *CollaborationRun, kind, role string, generation int, in CollaborationStepInput) (CollaborationStep, error) {
	if r.StepsUsed >= r.MaxSteps || len(in.Text) == 0 || len(in.Text) > collaboration.MaxInputBytes {
		return CollaborationStep{}, ErrChatInvalid
	}
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM collaboration_participants WHERE run_id=? AND role=? AND generation=? AND config_version=? AND state='active'", r.ID, role, generation, r.ConfigVersion).Scan(&count); err != nil {
		return CollaborationStep{}, err
	}
	if count != 1 {
		return CollaborationStep{}, ErrChatConflict
	}
	fp, raw, err := collaborationFingerprint(in)
	if err != nil {
		return CollaborationStep{}, err
	}
	var attempt int
	if err = tx.QueryRow("SELECT COALESCE(MAX(attempt),0)+1 FROM collaboration_steps WHERE run_id=? AND kind=?", r.ID, kind).Scan(&attempt); err != nil {
		return CollaborationStep{}, err
	}
	id := NewID()
	now := usageTimestamp(time.Now())
	st := CollaborationStep{ID: id, RunID: r.ID, Kind: kind, Role: role, ParticipantGeneration: generation, ConfigVersion: r.ConfigVersion, PlanVersion: r.PlanVersion, Attempt: attempt, TurnID: NewID(), State: "accepted", Revision: 1, SnapshotID: in.SnapshotID, CreatedAt: now, UpdatedAt: now}
	_, err = tx.Exec("INSERT INTO collaboration_steps(id,run_id,kind,role,participant_generation,config_version,plan_version,attempt,turn_id,input_fingerprint,input_json,state,snapshot_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, r.ID, kind, role, generation, r.ConfigVersion, r.PlanVersion, attempt, st.TurnID, fp, string(raw), st.State, st.SnapshotID, now, now)
	if err != nil {
		return CollaborationStep{}, err
	}
	return st, collaborationEventTx(tx, r.ID, "step", st)
}

// PublishCollaborationParticipants is called only after baseline publication
// and home preparation succeed. Metadata, sessions and first step commit as a
// unit; preparation failures never leave a runnable, half-configured task.
func (s *Store) PublishCollaborationParticipants(u User, id string, revision int64, coordinator, executor Session, in CollaborationStepInput) (CollaborationRun, CollaborationStep, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, CollaborationStep{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, id)
	if err != nil {
		return r, CollaborationStep{}, err
	}
	if r.Revision != revision || r.State != CollabPreparing || executor.User != u.Name {
		return r, CollaborationStep{}, ErrChatConflict
	}
	var raw string
	var cfg CollaborationConfig
	if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", id, r.ConfigVersion).Scan(&raw); err != nil {
		return r, CollaborationStep{}, err
	}
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return r, CollaborationStep{}, err
	}
	if !cfg.Workflow.IsDirect() && (coordinator.ID == executor.ID || coordinator.User != u.Name) {
		return r, CollaborationStep{}, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, id); err != nil {
		return r, CollaborationStep{}, err
	}
	generation := int(r.ConfigVersion)
	for _, role := range cfg.ActiveRoles() {
		sess := coordinator
		if role == collaboration.Executor {
			sess = executor
		}
		binding, _ := cfg.Roles.Binding(role)
		if !validChatKey(sess.ID) || sess.Agent != binding.AgentType || sess.AccountID != binding.AccountID || sess.DefaultModel != binding.Model || sess.ContainerID != "" || sess.ChatSession != "" || sess.Status != StatusStopped {
			return r, CollaborationStep{}, ErrChatInvalid
		}
		var n int
		if err = tx.QueryRow("SELECT count(*) FROM sessions WHERE id=?", sess.ID).Scan(&n); err != nil {
			return r, CollaborationStep{}, err
		}
		if n != 0 {
			return r, CollaborationStep{}, ErrChatConflict
		}
		if err = s.put(tx, sess); err != nil {
			return r, CollaborationStep{}, err
		}
		if _, err = tx.Exec("INSERT INTO collaboration_participants(run_id,role,generation,config_version,session_id,created_at) VALUES(?,?,?,?,?,?)", id, string(role), generation, r.ConfigVersion, sess.ID, usageTimestamp(time.Now())); err != nil {
			return r, CollaborationStep{}, err
		}
	}
	r.State, r.Phase = CollabPlanning, CollabPlanning
	var step CollaborationStep
	if cfg.Workflow != nil {
		kind := "plan"
		if cfg.Workflow.IsDirect() {
			kind = "execute"
			r.State, r.Phase = CollabExecuting, CollabExecuting
		}
		step, err = queueCollaborationStepTx(tx, &r, kind, in.SnapshotID)
	} else {
		step, err = addCollaborationStepTx(tx, &r, "plan", "coordinator", generation, in)
	}
	if err != nil {
		return r, step, err
	}
	var pause int
	if err = tx.QueryRow("SELECT pause_requested FROM collaborations WHERE id=?", id).Scan(&pause); err != nil {
		return r, step, err
	}
	if pause != 0 {
		pauseCollaboration(&r, "user_paused")
		if _, err = tx.Exec("UPDATE collaborations SET pause_requested=0 WHERE id=?", id); err != nil {
			return r, step, err
		}
	}
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, step, err
	}
	return r, step, tx.Commit()
}

func (s *Store) ClaimCollaborationStep(u User, run, id string, revision int64) (CollaborationStep, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationStep{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationStep{}, err
	}
	if r.State != CollabPlanning && r.State != CollabExecuting && r.State != CollabReviewing && r.State != CollabWaitingAnswer {
		return CollaborationStep{}, ErrChatConflict
	}
	if r.StepsUsed >= r.MaxSteps {
		return CollaborationStep{}, ErrChatConflict
	}
	var st CollaborationStep
	st, err = scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE id=? AND run_id=?", id, run))
	if err != nil {
		return st, err
	}
	if st.State != "accepted" || st.Revision != revision || st.ConfigVersion != r.ConfigVersion {
		return st, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return CollaborationStep{}, err
	}
	if paused, e := pauseForCollaborationTimeTx(tx, &r); e != nil {
		return st, e
	} else if paused {
		if e = saveCollaborationTx(tx, &r); e != nil {
			return st, e
		}
		if e = tx.Commit(); e != nil {
			return st, e
		}
		return st, collaboration.ErrState
	}
	if paused, e := pauseForCollaborationBudgetTx(tx, &r); e != nil {
		return st, e
	} else if paused {
		if e = saveCollaborationTx(tx, &r); e != nil {
			return st, e
		}
		if e = tx.Commit(); e != nil {
			return st, e
		}
		return st, collaboration.ErrState
	}
	res, err := tx.Exec("UPDATE collaboration_participants SET process_epoch=process_epoch+1 WHERE run_id=? AND role=? AND generation=? AND config_version=? AND state='active'", run, st.Role, st.ParticipantGeneration, r.ConfigVersion)
	if err != nil {
		return st, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return st, ErrChatConflict
	}
	st.State = "starting"
	st.Revision++
	st.UpdatedAt = usageTimestamp(time.Now())
	if _, err = tx.Exec("UPDATE collaboration_steps SET started_at=?,state=?,revision=?,updated_at=? WHERE id=?", st.UpdatedAt, st.State, st.Revision, st.UpdatedAt, id); err != nil {
		return st, err
	}
	r.StepsUsed++
	if err = saveCollaborationTx(tx, &r); err != nil {
		return st, err
	}
	if err = collaborationEventTx(tx, run, "step", st); err != nil {
		return st, err
	}
	return st, tx.Commit()
}

func (s *Store) AdvanceCollaborationStep(u User, run, id string, revision int64, state string) (CollaborationStep, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationStep{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationStep{}, err
	}
	st, err := scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE id=? AND run_id=?", id, run))
	if err != nil {
		return st, err
	}
	if st.Revision != revision || st.ConfigVersion != r.ConfigVersion || r.State == CollabCancelled || r.State == CollabDeleted || r.State == CollabUncertain || r.State == CollabPaused {
		return st, ErrChatConflict
	}
	if (state != "running" || st.State != "starting") && (state != "finalizing" || st.State != "running") {
		return st, ErrChatInvalid
	}
	if state == "running" && r.State == CollabCancelling {
		return st, ErrChatConflict
	}
	st.State = state
	st.Revision++
	st.UpdatedAt = usageTimestamp(time.Now())
	if _, err = tx.Exec("UPDATE collaboration_steps SET state=?,revision=?,updated_at=? WHERE id=?", state, st.Revision, st.UpdatedAt, id); err != nil {
		return st, err
	}
	if err = collaborationEventTx(tx, run, "step", st); err != nil {
		return st, err
	}
	return st, tx.Commit()
}

// RecoverCollaborations is run before Docker initialization. No recovered row
// confers scheduling permission, including rows accepted but not yet claimed.
func (s *Store) RecoverCollaborations() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE collaboration_decisions SET state='interrupted',revision=revision+1,updated_at=? WHERE state IN ('pending','claimed')", usageTimestamp(time.Now())); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE decision_attempts SET state='unknown',updated_at=? WHERE state='dispatched'", usageTimestamp(time.Now())); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaboration_applications SET state='uncertain' WHERE state IN ('staging','switching')"); err != nil {
		return err
	}
	helpers, err := tx.Query("SELECT run_id,snapshot_id,config_version FROM collaboration_verifications WHERE state='running'")
	if err != nil {
		return err
	}
	type unknown struct {
		run, snapshot string
		version       int64
	}
	var unknowns []unknown
	for helpers.Next() {
		var v unknown
		if err = helpers.Scan(&v.run, &v.snapshot, &v.version); err != nil {
			helpers.Close()
			return err
		}
		unknowns = append(unknowns, v)
	}
	err = helpers.Err()
	helpers.Close()
	if err != nil {
		return err
	}
	for _, v := range unknowns {
		if err = collaborationEventTx(tx, v.run, "verification", map[string]any{"snapshot_id": v.snapshot, "config_version": v.version, "state": "uncertain", "reason": "server_restarted"}); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE collaboration_verifications SET state='uncertain',updated_at=? WHERE state='running'", usageTimestamp(time.Now())); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT " + collaborationCols + " FROM collaborations WHERE state NOT IN ('completed','cancelled','failed','deleted','paused','uncertain')")
	if err != nil {
		return err
	}
	var runs []CollaborationRun
	for rows.Next() {
		r, e := scanCollaboration(rows)
		if e != nil {
			rows.Close()
			return e
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range runs {
		var active int
		if err = tx.QueryRow("SELECT count(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", r.ID).Scan(&active); err != nil {
			return err
		}
		r.ResumeState = r.State
		r.State, r.WaitReason = CollabPaused, "server_restarted"
		if active > 0 {
			r.State = CollabUncertain
		}
		var raw string
		var cfg CollaborationConfig
		if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", r.ID, r.ConfigVersion).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
			return err
		}
		// A crash has no reliable wall-clock stop timestamp. Charge the full
		// configured step reservation, never the subsequent user waiting time.
		reservation := "+" + strconv.Itoa(int(cfg.StepTimeout().Seconds())) + " seconds"
		if _, err = tx.Exec("UPDATE collaboration_steps SET ended_at=CASE WHEN started_at='' THEN '' ELSE datetime(started_at,?) END,state='uncertain',error_code='server_restarted',revision=revision+1,updated_at=? WHERE run_id=? AND state IN ('starting','running','finalizing')", reservation, usageTimestamp(time.Now()), r.ID); err != nil {
			return err
		}
		if err = saveCollaborationTx(tx, &r); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CollaborationByRequest(u User, id string) (CollaborationRun, error) {
	if !collaboration.ValidID(id) {
		return CollaborationRun{}, ErrChatInvalid
	}
	return scanCollaboration(s.db.QueryRow("SELECT "+collaborationCols+" FROM collaborations WHERE user=? AND user_created_at=? AND request_id=? AND EXISTS(SELECT 1 FROM users WHERE name=? AND created_at=?)", u.Name, ownerEpoch(u), id, u.Name, ownerEpoch(u)))
}

func (s *Store) IsCollaborationSession(id string) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM collaboration_participants WHERE session_id=?", id).Scan(&n)
	return n > 0, err
}

// clearUserCollaborationsTx clears task content in the user's deletion
// transaction. IDs and accounting remain; queued/late runners lose their CAS.
func clearUserCollaborationsTx(tx *sql.Tx, user string) error {
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM collaboration_applications WHERE run_id IN (SELECT id FROM collaborations WHERE user=?) AND state IN ('staging','switching','uncertain')", user).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return ErrChatConflict
	}
	for _, query := range []string{
		"DELETE FROM collaboration_review_evidence WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_configs SET config_json='{}' WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_actions SET action_json='null',result_json='{}',fingerprint='',state='deleted',revision=revision+1 WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_messages SET body_json='null' WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_questions SET state='retired' WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_verifications SET command='',output='',argv_json='[]' WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_events SET body_json='null' WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_steps SET input_json='null',input_fingerprint='',state='cancelled',revision=revision+1 WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_artifacts SET state='revoked' WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaboration_participants SET state='revoked',provider_session='',context_provider='',context_seq=0 WHERE run_id IN (SELECT id FROM collaborations WHERE user=?)",
		"UPDATE collaborations SET state='deleted',request_json='null',fingerprint='',revision=revision+1 WHERE user=?",
	} {
		if _, err := tx.Exec(query, user); err != nil {
			return err
		}
	}
	return nil
}
