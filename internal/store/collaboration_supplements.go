package store

import (
	"agentbox/internal/collaboration"
	"database/sql"
	"encoding/json"
	"strings"
)

func supplementCollaborationTx(tx *sql.Tx, r *CollaborationRun, id string, payload json.RawMessage) error {
	if err := collaborationDecisionReadyTx(tx, r.ID); err != nil {
		return err
	}
	var input struct {
		Text string `json:"text"`
	}
	if collaboration.Decode(payload, &input, 24000) != nil || strings.TrimSpace(input.Text) == "" || len(input.Text) > 20000 {
		return ErrChatInvalid
	}
	switch r.State {
	case CollabPlanning, CollabExecuting, CollabReviewing, CollabWaitingAnswer, CollabAwaitingPlan, CollabWaitingUser, CollabPaused:
	default:
		return collaboration.ErrState
	}
	// Unknown execution and exhausted limits still require their existing recovery.
	if r.State == CollabPaused && r.WaitReason != "user_paused" && r.WaitReason != "requirements_changed" {
		return collaboration.ErrState
	}
	instruction := "User supplement. Revise the plan before implementing this change."
	if r.Request.Workflow.IsDirect() {
		instruction = "User supplement. Reconsider the task and implement this change before delivery."
	}
	body, _ := json.Marshal(map[string]any{"text": input.Text, "instruction": instruction})
	if _, err := tx.Exec(`INSERT INTO collaboration_messages(run_id,seq,id,step_id,sender_role,type,config_version,plan_version,body_json,created_at) SELECT ?,COALESCE(MAX(seq),0)+1,?,'','user','system',?,?,?,? FROM collaboration_messages WHERE run_id=?`, r.ID, id, r.ConfigVersion, r.PlanVersion, string(body), r.UpdatedAt, r.ID); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO collaboration_supplements(run_id,action_id) VALUES(?,?)", r.ID, id); err != nil {
		return err
	}
	if err := collaborationEventTx(tx, r.ID, "message", map[string]any{"id": id, "sender_role": "user", "type": "system", "body": json.RawMessage(body)}); err != nil {
		return err
	}
	return nil
}

// Incorporate only at a quiescent, fully accounted step boundary. Never alter
// a claimed step's frozen input, resurrect failed work or bypass verification.
func (s *Store) IncorporateCollaborationSupplements(u User, run string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return false, err
	}
	var count int
	if err = tx.QueryRow("SELECT COUNT(*) FROM collaboration_supplements WHERE run_id=? AND applied_version=0", run).Scan(&count); err != nil || count == 0 {
		return false, err
	}
	switch r.State {
	case CollabPlanning, CollabExecuting, CollabReviewing, CollabWaitingAnswer, CollabAwaitingPlan, CollabWaitingUser:
	case CollabPaused:
		if r.WaitReason != "user_paused" && r.WaitReason != "verification_required" && r.WaitReason != "delivery_required" {
			return false, nil
		}
	default:
		return false, nil
	}
	if err = tx.QueryRow("SELECT COUNT(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", run).Scan(&count); err != nil || count != 0 {
		return false, err
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return false, err
	}
	var raw string
	if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", run, r.ConfigVersion).Scan(&raw); err != nil {
		return false, err
	}
	r.State, r.WaitReason = CollabPaused, "requirements_changed"
	if err = configureCollaborationTx(tx, &r, json.RawMessage(raw)); err != nil {
		return false, err
	}
	if _, err = tx.Exec("UPDATE collaboration_supplements SET applied_version=? WHERE run_id=? AND applied_version=0", r.ConfigVersion, run); err != nil {
		return false, err
	}
	if err = saveCollaborationTx(tx, &r); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
