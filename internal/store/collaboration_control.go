package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"agentbox/internal/collaboration"
)

type CollaborationParticipant struct {
	Role          string `json:"role"`
	Generation    int    `json:"generation"`
	ConfigVersion int64  `json:"config_version"`
	SessionID     string `json:"session_id"`
	ContainerID   string `json:"container_id"`
	ProcessEpoch  int64  `json:"process_epoch"`
	StoppedEpoch  int64  `json:"stopped_epoch"`
	State         string `json:"state"`
}

// PublicSessions excludes managed participants without changing All/List used
// by credentials, usage, capacity and lifecycle maintenance.
func (s *Store) PublicSessions(user string) ([]Session, error) {
	rows, err := s.db.Query("SELECT "+sessionCols+" FROM sessions WHERE user=? AND NOT EXISTS(SELECT 1 FROM collaboration_participants WHERE session_id=sessions.id) ORDER BY created_at DESC", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, e := scanSession(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) CollaborationParticipants(u User, run string) ([]CollaborationParticipant, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT p.role,p.generation,p.config_version,p.session_id,COALESCE(s.container_id,''),p.process_epoch,p.stopped_epoch,p.state FROM collaboration_participants p LEFT JOIN sessions s ON s.id=p.session_id WHERE p.run_id=? ORDER BY p.generation,p.role`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaborationParticipant{}
	for rows.Next() {
		var p CollaborationParticipant
		if err = rows.Scan(&p.Role, &p.Generation, &p.ConfigVersion, &p.SessionID, &p.ContainerID, &p.ProcessEpoch, &p.StoppedEpoch, &p.State); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func collaborationStoppedTx(tx *sql.Tx, run string) error {
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM collaboration_participants WHERE run_id=? AND process_epoch!=stopped_epoch", run).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return collaboration.ErrState
	}
	if err := tx.QueryRow("SELECT count(*) FROM collaboration_verifications WHERE run_id=? AND state IN ('running','uncertain')", run).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return collaboration.ErrState
	}
	return nil
}

// ConfirmCollaborationStopped is a runtime-only acknowledgement. The caller
// must hold workspace lifecycle locks and inspect/stop these exact containers
// before calling it. HTTP input must never be used as stop evidence. Epoch and
// container comparisons prevent a delayed observation from acknowledging a
// later process; running tasks must use normal per-step finalization instead.
func (s *Store) ConfirmCollaborationStopped(u User, run string, revision int64, observations []CollaborationParticipant) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	if r.Revision != revision || (r.State != CollabPaused && r.State != CollabUncertain && r.State != CollabCancelling) {
		return ErrChatConflict
	}
	for _, p := range observations {
		res, err := tx.Exec(`UPDATE collaboration_participants SET stopped_epoch=process_epoch WHERE run_id=? AND session_id=? AND generation=? AND process_epoch=? AND EXISTS(SELECT 1 FROM sessions WHERE id=? AND container_id=?)`, run, p.SessionID, p.Generation, p.ProcessEpoch, p.SessionID, p.ContainerID)
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
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return err
	}
	if err = collaborationEventTx(tx, run, "processes_stopped", map[string]any{"revision": revision}); err != nil {
		return err
	}
	return tx.Commit()
}

// Cancellation finalization does not imply that files or costs were rolled
// back. Uncertain history/accounting must be reconciled before this call.
func (s *Store) FinalizeCollaborationCancellation(u User, run string, revision int64) (CollaborationRun, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return r, err
	}
	if r.Revision != revision || r.State != CollabCancelling {
		return r, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return r, err
	}
	var active int
	if err = tx.QueryRow("SELECT count(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", run).Scan(&active); err != nil {
		return r, err
	}
	if active != 0 {
		return r, collaboration.ErrState
	}
	if _, err = tx.Exec("UPDATE collaboration_steps SET state='cancelled',revision=revision+1,updated_at=? WHERE run_id=? AND state='accepted'", usageTimestamp(time.Now()), run); err != nil {
		return r, err
	}
	r.State, r.WaitReason = CollabCancelled, "user_cancelled"
	r.NextKind = ""
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

func pauseCollaboration(r *CollaborationRun, reason string) {
	if r.State != CollabPaused && r.State != CollabUncertain {
		r.ResumeState = r.State
	}
	r.State, r.WaitReason = CollabPaused, reason
}

func resumeCollaborationTx(tx *sql.Tx, r *CollaborationRun) error {
	if r.State != CollabPaused {
		return collaboration.ErrState
	}
	if err := collaborationStoppedTx(tx, r.ID); err != nil {
		return err
	}
	// These reasons require an explicit repair, verification, or reconfigure
	// operation. Resume must not turn unknown/invalid output into success.
	switch r.WaitReason {
	case "handoff_invalid", "verification_required", "delivery_required", "verification_failed", "runtime_limit", "repair_limit", "budget_limit", "usage_incomplete", "context_limit", "artifacts_missing", "decision_persistence_failed":
		return collaboration.ErrState
	}
	var active int
	if err := tx.QueryRow("SELECT count(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", r.ID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return collaboration.ErrState
	}
	state := r.ResumeState
	switch state {
	case CollabAccepted, CollabPlanning, CollabExecuting, CollabReviewing, CollabWaitingAnswer, CollabWaitingUser, CollabAwaitingPlan:
	default:
		return collaboration.ErrState
	}
	if r.StepsUsed >= r.MaxSteps && state != CollabWaitingUser && state != CollabAwaitingPlan {
		return collaboration.ErrState
	}
	r.State, r.WaitReason, r.ResumeState = state, "", ""
	if r.NextKind == "verification_required" || r.NextKind == "delivery_required" {
		gate := r.NextKind
		r.NextKind = ""
		pauseCollaboration(r, gate)
	} else if r.NextKind != "" {
		var snapshot string
		if err := tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", r.ID).Scan(&snapshot); err != nil {
			return err
		}
		if _, err := queueCollaborationStepTx(tx, r, r.NextKind, snapshot); err != nil {
			return err
		}
		r.NextKind = ""
	}
	_, err := tx.Exec("UPDATE collaborations SET pause_requested=0 WHERE id=?", r.ID)
	return err
}

func configureCollaborationTx(tx *sql.Tx, r *CollaborationRun, payload json.RawMessage) error {
	if err := collaborationDecisionReadyTx(tx, r.ID); err != nil {
		return err
	}
	var cfg CollaborationConfig
	if collaboration.Decode(payload, &cfg, collaboration.MaxHandoffBytes) != nil || !cfg.CollaborationLimits.Valid() || cfg.Roles.Validate() != nil || cfg.MaxSteps < 1 || cfg.MaxSteps > 100 || cfg.MaxSteps <= r.StepsUsed || cfg.MaxRepairs < 0 || cfg.MaxRepairs > 20 || cfg.BudgetMicroUSD < 0 || len(cfg.VerificationCommand) > 8192 || strings.ContainsRune(cfg.VerificationCommand, 0) {
		return ErrChatInvalid
	}
	if r.State != CollabAccepted && r.State != CollabPaused && r.State != CollabWaitingUser && r.State != CollabAwaitingPlan {
		return collaboration.ErrState
	}
	if err := collaborationStoppedTx(tx, r.ID); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow("SELECT count(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", r.ID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return collaboration.ErrState
	}
	var oldRaw string
	var old CollaborationConfig
	if err := tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", r.ID, r.ConfigVersion).Scan(&oldRaw); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(oldRaw), &old); err != nil {
		return err
	}
	if !sameCollaborationWorkflow(old.Workflow, cfg.Workflow) {
		return ErrChatInvalid
	}
	// Compare workflow values, not JSON-unmarshal pointer identities.
	cfg.Workflow = old.Workflow
	if old == cfg && r.WaitReason != "requirements_changed" && r.WaitReason != "execution_failed" && r.WaitReason != "result_reviewed" && r.WaitReason != "verification_required" && r.WaitReason != "verification_failed" && r.WaitReason != "delivery_required" && r.WaitReason != "instance_restored" && r.WaitReason != "handoff_invalid" {
		return nil
	}
	if err := reserveDirectReconfigurationTx(tx, r, old, cfg); err != nil {
		return err
	}
	// Every binding change has a fresh home/provider thread. Old participants,
	// their transcripts and usage remain attributable to the old generation.
	r.ConfigVersion++
	r.MaxSteps = cfg.MaxSteps
	r.MaxRepairs = cfg.MaxRepairs
	r.BudgetMicroUSD = cfg.BudgetMicroUSD
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO collaboration_configs(run_id,version,config_json,created_at) VALUES(?,?,?,?)", r.ID, r.ConfigVersion, string(encoded), usageTimestamp(time.Now())); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaboration_participants SET state='revoked' WHERE run_id=?", r.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaboration_steps SET state='cancelled',revision=revision+1,updated_at=? WHERE run_id=? AND state='accepted'", usageTimestamp(time.Now()), r.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaboration_questions SET state='retired' WHERE run_id=? AND state='open'", r.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaborations SET pending_question='',pause_requested=0 WHERE id=?", r.ID); err != nil {
		return err
	}
	r.State, r.Phase, r.WaitReason, r.NextKind, r.ResumeState = CollabAccepted, CollabPreparing, "configuration_changed", "", ""
	return collaborationEventTx(tx, r.ID, "configuration", map[string]any{"version": r.ConfigVersion, "configuration": cfg, "context_rebuilt": true})
}
