package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentbox/internal/collaboration"
)

func sameCollaborationWorkflow(a, b *CollaborationWorkflow) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (c CollaborationConfig) ActiveRoles() []collaboration.Role {
	if c.Workflow.IsDirect() {
		return []collaboration.Role{collaboration.Executor}
	}
	return []collaboration.Role{collaboration.Coordinator, collaboration.Executor}
}

func (c CollaborationCreateInput) ActiveRoles() []collaboration.Role {
	return (CollaborationConfig{Workflow: c.Workflow}).ActiveRoles()
}

func collaborationConfigTx(tx *sql.Tx, r *CollaborationRun) (CollaborationConfig, error) {
	var raw string
	var cfg CollaborationConfig
	err := tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", r.ID, r.ConfigVersion).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &cfg)
	}
	return cfg, err
}

// Rebinding a direct task may create a fresh native thread, but cannot turn a
// requested repair into an uncounted initial execution. A pending, unstarted
// repair keeps its existing reservation. Explicit new requirements or a changed
// acceptance command are a new scope; the global step/runtime/budget caps stay.
func reserveDirectReconfigurationTx(tx *sql.Tx, r *CollaborationRun, old, cfg CollaborationConfig) error {
	if !cfg.Workflow.IsDirect() || r.WaitReason == "requirements_changed" || cfg.VerificationCommand != old.VerificationCommand {
		return nil
	}
	var results int
	if err := tx.QueryRow("SELECT COUNT(*) FROM collaboration_messages WHERE run_id=? AND config_version=? AND type='result'", r.ID, r.ConfigVersion).Scan(&results); err != nil {
		return err
	}
	if results == 0 && r.WaitReason != "handoff_invalid" && r.NextKind != "repair" {
		return nil
	}
	var repairs, reserved int
	if err := tx.QueryRow("SELECT repairs_used FROM collaborations WHERE id=?", r.ID).Scan(&repairs); err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT COUNT(*) FROM collaboration_steps WHERE run_id=? AND config_version=? AND kind='repair' AND (state='accepted' OR (state='failed' AND ?='execution_failed'))", r.ID, r.ConfigVersion, r.WaitReason).Scan(&reserved); err != nil {
		return err
	}
	if reserved > 0 {
		if cfg.MaxRepairs < repairs {
			return collaboration.ErrState
		}
		return nil
	}
	if cfg.MaxRepairs <= repairs {
		return collaboration.ErrState
	}
	_, err := tx.Exec("UPDATE collaborations SET repairs_used=repairs_used+1 WHERE id=?", r.ID)
	return err
}

func freezeCollaborationReviewEvidenceTx(tx *sql.Tx, r *CollaborationRun, snapshot string, evidence *collaboration.ReviewEvidence) error {
	if evidence == nil || evidence.Validate(snapshot, r.PlanVersion) != nil {
		return ErrChatInvalid
	}
	for _, artifact := range []struct{ id, hash, kind string }{{evidence.BaselineID, evidence.BaselineSHA256, "baseline"}, {evidence.SnapshotID, evidence.SnapshotSHA256, "snapshot"}} {
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM collaboration_artifacts WHERE run_id=? AND id=? AND sha256=? AND kind=? AND state='ready'", r.ID, artifact.id, artifact.hash, artifact.kind).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return ErrChatConflict
		}
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	var previous string
	err = tx.QueryRow("SELECT evidence_json FROM collaboration_review_evidence WHERE run_id=? AND snapshot_id=? AND config_version=?", r.ID, snapshot, r.ConfigVersion).Scan(&previous)
	if err == nil {
		if previous != string(raw) {
			return ErrChatConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.Exec("INSERT INTO collaboration_review_evidence(run_id,snapshot_id,config_version,evidence_json,created_at) VALUES(?,?,?,?,?)", r.ID, snapshot, r.ConfigVersion, string(raw), usageTimestamp(time.Now()))
	return err
}

func collaborationReviewEvidenceTx(tx *sql.Tx, r *CollaborationRun, snapshot string) (*collaboration.ReviewEvidence, error) {
	var raw string
	err := tx.QueryRow("SELECT evidence_json FROM collaboration_review_evidence WHERE run_id=? AND snapshot_id=? AND config_version=?", r.ID, snapshot, r.ConfigVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) && r.Request.Workflow == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var evidence collaboration.ReviewEvidence
	if err = json.Unmarshal([]byte(raw), &evidence); err != nil {
		return nil, err
	}
	if err = evidence.Validate(snapshot, r.PlanVersion); err != nil {
		return nil, err
	}
	return &evidence, nil
}

// Direct work retains the same external verification, publication and explicit
// application gates. Outstanding work never becomes a successful delivery.
func advanceDirectCollaborationTx(tx *sql.Tx, r *CollaborationRun, snapshot string, v CollaborationVerification) error {
	var raw string
	if err := tx.QueryRow("SELECT body_json FROM collaboration_messages WHERE run_id=? AND type='result' AND config_version=? ORDER BY seq DESC LIMIT 1", r.ID, r.ConfigVersion).Scan(&raw); err != nil {
		return err
	}
	var result collaboration.Result
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return err
	}
	r.State, r.Phase, r.WaitReason, r.ResumeState = CollabExecuting, CollabExecuting, "", ""
	if v.State != "failed" && len(result.Remaining) == 0 {
		pauseCollaboration(r, "delivery_required")
		return nil
	}
	var repairs int
	if err := tx.QueryRow("SELECT repairs_used FROM collaborations WHERE id=?", r.ID).Scan(&repairs); err != nil {
		return err
	}
	if repairs >= r.MaxRepairs {
		r.NextKind = "repair"
		pauseCollaboration(r, "repair_limit")
		return nil
	}
	if r.StepsUsed >= r.MaxSteps {
		r.NextKind = "repair"
		pauseCollaboration(r, "step_limit")
		return nil
	}
	if _, err := tx.Exec("UPDATE collaborations SET repairs_used=repairs_used+1 WHERE id=?", r.ID); err != nil {
		return err
	}
	_, err := queueCollaborationStepTx(tx, r, "repair", snapshot)
	return err
}
