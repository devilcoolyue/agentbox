package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentbox/internal/collaboration"
)

type CollaborationVerification struct {
	ExitCode        *int     `json:"exit_code"`
	OutputTruncated bool     `json:"output_truncated"`
	Argv            []string `json:"argv"`
	HelperID        string   `json:"helper_id"`
	RunID           string   `json:"run_id"`
	SnapshotID      string   `json:"snapshot_id"`
	ConfigVersion   int64    `json:"config_version"`
	Command         string   `json:"command"`
	State           string   `json:"state"`
	Output          string   `json:"output"`
	DurationMS      int64    `json:"duration_ms"`
}

// BeginCollaborationVerification claims external test execution once. Crash
// recovery fences running verifications; retrying an HTTP read never reruns a
// project script. No test command means an explicitly unverified model review.
func (s *Store) BeginCollaborationVerification(u User, run string, revision int64) (CollaborationVerification, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationVerification{}, false, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationVerification{}, false, err
	}
	if r.Revision != revision || r.State != CollabPaused || r.WaitReason != "verification_required" {
		return CollaborationVerification{}, false, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return CollaborationVerification{}, false, err
	}
	var v CollaborationVerification
	v.RunID = run
	v.ConfigVersion = r.ConfigVersion
	if err = tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", run).Scan(&v.SnapshotID); err != nil {
		return v, false, err
	}
	if !collaboration.ValidID(v.SnapshotID) {
		return v, false, ErrChatInvalid
	}
	var raw string
	var cfg CollaborationConfig
	if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", run, r.ConfigVersion).Scan(&raw); err != nil {
		return v, false, err
	}
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return v, false, err
	}
	v.Command = cfg.VerificationCommand
	err = tx.QueryRow("SELECT state,output,duration_ms,helper_id FROM collaboration_verifications WHERE run_id=? AND snapshot_id=? AND config_version=?", run, v.SnapshotID, v.ConfigVersion).Scan(&v.State, &v.Output, &v.DurationMS, &v.HelperID)
	if err == nil {
		return v, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return v, false, err
	}
	v.HelperID = NewCollaborationID()
	v.State = "running"
	if v.Command == "" {
		v.State = "not_requested"
	}
	now := usageTimestamp(time.Now())
	if _, err = tx.Exec("INSERT INTO collaboration_verifications(run_id,snapshot_id,config_version,command,state,helper_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", run, v.SnapshotID, v.ConfigVersion, v.Command, v.State, v.HelperID, now, now); err != nil {
		return v, false, err
	}
	if err = collaborationEventTx(tx, run, "verification", v); err != nil {
		return v, false, err
	}
	if err = tx.Commit(); err != nil {
		return v, false, err
	}
	return v, true, nil
}

func (s *Store) FinishCollaborationVerification(u User, v CollaborationVerification, state, output string, duration time.Duration) error {
	if (state != "completed" && state != "failed" && state != "uncertain") || len(output) > 128<<10 || duration < 0 {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, v.RunID)
	if err != nil {
		return err
	}
	if r.ConfigVersion != v.ConfigVersion || r.State == CollabDeleted {
		return ErrChatConflict
	}
	argv, err := json.Marshal(v.Argv)
	if err != nil {
		return err
	}
	res, err := tx.Exec("UPDATE collaboration_verifications SET exit_code=?,output_truncated=?,argv_json=?,state=?,output=?,duration_ms=?,updated_at=? WHERE run_id=? AND snapshot_id=? AND config_version=? AND helper_id=? AND state='running'", v.ExitCode, v.OutputTruncated, string(argv), state, output, duration.Milliseconds(), usageTimestamp(time.Now()), v.RunID, v.SnapshotID, v.ConfigVersion, v.HelperID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrChatConflict
	}
	v.State, v.Output, v.DurationMS = state, output, duration.Milliseconds()
	if err = collaborationEventTx(tx, v.RunID, "verification", v); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeliverCollaboration(u User, run string, revision int64, snapshot string) (CollaborationRun, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return r, err
	}
	if r.Revision != revision || r.State != CollabPaused || r.WaitReason != "delivery_required" {
		return r, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return r, err
	}
	var current, state string
	if err = tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", run).Scan(&current); err != nil {
		return r, err
	}
	if current != snapshot {
		return r, ErrChatConflict
	}
	if err = tx.QueryRow("SELECT state FROM collaboration_verifications WHERE run_id=? AND snapshot_id=? AND config_version=?", run, snapshot, r.ConfigVersion).Scan(&state); err != nil {
		return r, err
	}
	if state != "completed" && state != "not_requested" {
		return r, collaboration.ErrState
	}
	var ready int
	if err = tx.QueryRow("SELECT count(*) FROM collaboration_artifacts WHERE id=? AND run_id=? AND state='ready'", snapshot, run).Scan(&ready); err != nil {
		return r, err
	}
	if ready != 1 {
		return r, collaboration.ErrState
	}
	r.State, r.WaitReason = CollabCompleted, ""
	if state == "not_requested" {
		r.WaitReason = "model_review_only"
		if r.Request.Workflow.IsDirect() {
			r.WaitReason = "verification_not_requested"
		}
	}
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// UncertainCollaborationVerifications is runtime recovery input, never client
// supplied stop evidence. Running claims remain fenced until their owner exits
// or startup recovery marks them uncertain.
func (s *Store) UncertainCollaborationVerifications(u User, run string) ([]CollaborationVerification, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT snapshot_id,config_version,helper_id,command,output,duration_ms FROM collaboration_verifications WHERE run_id=? AND state='uncertain'", run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaborationVerification{}
	for rows.Next() {
		v := CollaborationVerification{RunID: run, State: "uncertain"}
		if err = rows.Scan(&v.SnapshotID, &v.ConfigVersion, &v.HelperID, &v.Command, &v.Output, &v.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ConfirmCollaborationVerificationStopped requires Docker removal/not-found
// proof for the persisted helper identity. Unknown outcomes become failed,
// never passed, and the original claim cannot run again.
func (s *Store) ConfirmCollaborationVerificationStopped(u User, v CollaborationVerification) error {
	if !collaboration.ValidID(v.HelperID) {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, v.RunID)
	if err != nil {
		return err
	}
	if r.State != CollabPaused && r.State != CollabUncertain && r.State != CollabCancelling {
		return ErrChatConflict
	}
	res, err := tx.Exec("UPDATE collaboration_verifications SET state='failed',updated_at=? WHERE run_id=? AND snapshot_id=? AND config_version=? AND helper_id=? AND state='uncertain'", usageTimestamp(time.Now()), v.RunID, v.SnapshotID, v.ConfigVersion, v.HelperID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrChatConflict
	}
	if err = collaborationEventTx(tx, v.RunID, "verification_stopped", map[string]any{"snapshot_id": v.SnapshotID, "config_version": v.ConfigVersion, "state": "failed", "reason": "outcome_unknown"}); err != nil {
		return err
	}
	return tx.Commit()
}

// QueueCollaborationReview freezes platform evidence into the review input.
// A failed test may be reviewed for repair, but an unknown helper cannot advance.
func (s *Store) QueueCollaborationReview(u User, run string, revision int64, evidence ...*collaboration.ReviewEvidence) (CollaborationRun, error) {
	if len(evidence) > 1 {
		return CollaborationRun{}, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return r, err
	}
	if r.Revision != revision || r.State != CollabPaused || r.WaitReason != "verification_required" {
		return r, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return r, err
	}
	var snapshot string
	if err = tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", run).Scan(&snapshot); err != nil {
		return r, err
	}
	v, err := collaborationVerificationTx(tx, run, snapshot, r.ConfigVersion)
	if err != nil {
		return r, err
	}
	if v.State != "completed" && v.State != "failed" && v.State != "not_requested" {
		return r, collaboration.ErrState
	}
	if r.Request.Workflow.IsDirect() {
		if err = advanceDirectCollaborationTx(tx, &r, snapshot, v); err != nil {
			return r, err
		}
		if err = saveCollaborationTx(tx, &r); err != nil {
			return r, err
		}
		return r, tx.Commit()
	}
	if len(evidence) == 1 {
		if err = freezeCollaborationReviewEvidenceTx(tx, &r, snapshot, evidence[0]); err != nil {
			return r, err
		}
	} else if r.Request.Workflow != nil {
		return r, ErrChatInvalid
	}
	r.State, r.Phase, r.WaitReason, r.ResumeState = CollabReviewing, CollabReviewing, "", ""
	if r.StepsUsed >= r.MaxSteps {
		r.NextKind = "review"
		pauseCollaboration(&r, "step_limit")
	} else {
		if _, err = queueCollaborationStepTx(tx, &r, "review", snapshot); err != nil {
			return r, err
		}
	}
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, err
	}
	return r, tx.Commit()
}
func collaborationVerificationTx(tx *sql.Tx, run, snapshot string, version int64) (CollaborationVerification, error) {
	v := CollaborationVerification{RunID: run, SnapshotID: snapshot, ConfigVersion: version}
	var argv string
	err := tx.QueryRow("SELECT command,state,output,duration_ms,helper_id,exit_code,output_truncated,argv_json FROM collaboration_verifications WHERE run_id=? AND snapshot_id=? AND config_version=?", run, snapshot, version).Scan(&v.Command, &v.State, &v.Output, &v.DurationMS, &v.HelperID, &v.ExitCode, &v.OutputTruncated, &argv)
	if err == nil {
		err = json.Unmarshal([]byte(argv), &v.Argv)
	}
	return v, err
}
