package store

import (
	"agentbox/internal/collaboration"
	"database/sql"
	"encoding/json"
)

// A malformed final answer can be reformatted by the same participant without
// granting another writable implementation turn. The original transcript and
// already published snapshot remain the source of truth.
func repairCollaborationHandoffTx(tx *sql.Tx, r *CollaborationRun) error {
	if r.State != CollabPaused || r.WaitReason != "handoff_invalid" || r.StepsUsed >= r.MaxSteps {
		return collaboration.ErrState
	}
	if err := collaborationStoppedTx(tx, r.ID); err != nil {
		return err
	}
	st, err := scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE run_id=? AND config_version=? AND state='completed' ORDER BY updated_at DESC,id DESC LIMIT 1", r.ID, r.ConfigVersion))
	if err != nil {
		return err
	}
	kind := st.Kind
	if kind == "handoff_repair" {
		var raw string
		var input CollaborationStepInput
		if err = tx.QueryRow("SELECT input_json FROM collaboration_steps WHERE id=?", st.ID).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &input); err != nil {
			return err
		}
		kind = input.RepairKind
	}
	var snapshot string
	if err = tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", r.ID).Scan(&snapshot); err != nil {
		return err
	}
	input, err := collaborationStepPromptTx(tx, r, collaboration.Role(st.Role), kind, snapshot, st.ParticipantGeneration, true)
	if err != nil {
		return err
	}
	input.Text = "HANDOFF REPAIR ONLY: Your previous final answer did not match the required schema. Return a corrected handoff describing only work already performed. Do not repeat implementation, tests, tools or other side effects. The project is read-only for this turn.\n" + input.Text
	input.RepairKind = kind
	_, err = addCollaborationStepTx(tx, r, "handoff_repair", st.Role, st.ParticipantGeneration, input)
	if err != nil {
		return err
	}
	switch kind {
	case "plan":
		r.State = CollabPlanning
	case "execute", "repair":
		r.State = CollabExecuting
	case "review":
		r.State = CollabReviewing
	case "clarify":
		r.State = CollabWaitingAnswer
	default:
		return ErrChatInvalid
	}
	r.WaitReason, r.NextKind, r.ResumeState = "", "", ""
	return nil
}
