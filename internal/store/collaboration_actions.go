package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentbox/internal/collaboration"
)

// ApplyCollaborationAction commits the state transition and its receipt in the
// same transaction. Replayed actions return their original frozen result even
// when the task has since advanced; they never grant scheduling rights.
func (s *Store) ApplyCollaborationAction(u User, run, id string, in CollaborationActionInput) (CollaborationAction, bool, error) {
	if !collaboration.ValidID(id) || in.ExpectedRevision < 1 || in.Scope == "" || len(in.Payload) > collaboration.MaxHandoffBytes {
		return CollaborationAction{}, false, ErrChatInvalid
	}
	fp, raw, err := collaborationFingerprint(in)
	if err != nil {
		return CollaborationAction{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationAction{}, false, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationAction{}, false, err
	}
	if in.Scope != r.Scope {
		return CollaborationAction{}, false, ErrChatConflict
	}
	var receipt CollaborationAction
	var result, storedFingerprint string
	err = tx.QueryRow("SELECT state,result_json,revision,fingerprint FROM collaboration_actions WHERE run_id=? AND action_id=?", run, id).Scan(&receipt.State, &result, &receipt.Revision, &storedFingerprint)
	if err == nil {
		receipt.ActionID = id
		receipt.Result = json.RawMessage(result)
		if receipt.State == "abandoned" || r.State == CollabDeleted {
			return receipt, false, ErrChatGone
		}
		if fp != storedFingerprint {
			return receipt, false, ErrChatConflict
		}
		return receipt, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, false, err
	}
	if r.Request == nil || in.ExpectedRevision != r.Revision {
		return receipt, false, ErrChatConflict
	}
	if r.State == CollabCompleted || r.State == CollabCancelled || r.State == CollabDeleted || r.State == CollabFailed {
		return receipt, false, collaboration.ErrState
	}
	var active int
	if err = tx.QueryRow("SELECT count(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", run).Scan(&active); err != nil {
		return receipt, false, err
	}
	switch in.Action {
	case "supplement":
		if err = supplementCollaborationTx(tx, &r, id, in.Payload); err != nil {
			return receipt, false, err
		}
	case "pause":
		if len(in.Payload) > 0 {
			return receipt, false, ErrChatInvalid
		}
		if r.State == CollabCancelling || r.State == CollabUncertain {
			return receipt, false, collaboration.ErrState
		}
		if r.State == CollabPaused {
			break
		}
		// Preparation performs external filesystem writes too. Let its owner
		// reach a checkpoint before transferring control to a user action.
		if active > 0 || r.State == CollabPreparing {
			if _, err = tx.Exec("UPDATE collaborations SET pause_requested=1 WHERE id=?", run); err != nil {
				return receipt, false, err
			}
			r.WaitReason = "pause_requested"
		} else {
			pauseCollaboration(&r, "user_paused")
		}
	case "resume":
		if len(in.Payload) > 0 {
			return receipt, false, ErrChatInvalid
		}
		if err = resumeCollaborationTx(tx, &r); err != nil {
			return receipt, false, err
		}
	case "configure":
		if err = configureCollaborationTx(tx, &r, in.Payload); err != nil {
			return receipt, false, err
		}
	case "repair_handoff":
		if len(in.Payload) > 0 {
			return receipt, false, ErrChatInvalid
		}
		if err = repairCollaborationHandoffTx(tx, &r); err != nil {
			return receipt, false, err
		}
	case "review":
		if err = reviewCollaborationTx(tx, &r, in.Payload); err != nil {
			return receipt, false, err
		}
	case "answer":
		if active != 0 {
			return receipt, false, collaboration.ErrState
		}
		if err = answerCollaborationTx(tx, &r, in.Payload); err != nil {
			return receipt, false, err
		}
	case "cancel":
		if len(in.Payload) > 0 {
			return receipt, false, ErrChatInvalid
		}
		r.State = CollabCancelling
		r.WaitReason = "user_cancelled"
		// Actual process stop is reconciled separately. This action never
		// claims a cancelled task or grants a second writer prematurely.
	case "confirm_plan":
		var payload struct {
			PlanVersion int64 `json:"plan_version"`
		}
		if collaboration.Decode(in.Payload, &payload, 1024) != nil || payload.PlanVersion != r.PlanVersion {
			return receipt, false, ErrChatInvalid
		}
		if r.State != CollabAwaitingPlan || active > 0 {
			return receipt, false, collaboration.ErrState
		}
		if err = collaborationStoppedTx(tx, run); err != nil {
			return receipt, false, err
		}
		r.State, r.Phase, r.WaitReason = CollabExecuting, CollabExecuting, ""
		var snapshot string
		if err = tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", run).Scan(&snapshot); err != nil {
			return receipt, false, err
		}
		if _, err = queueCollaborationStepTx(tx, &r, "execute", snapshot); err != nil {
			return receipt, false, err
		}
	default:
		return receipt, false, collaboration.ErrState
	}
	if err = saveCollaborationTx(tx, &r); err != nil {
		return receipt, false, err
	}
	view := r
	view.Request = nil
	encoded, err := json.Marshal(view)
	if err != nil {
		return receipt, false, err
	}
	now := usageTimestamp(time.Now())
	receipt = CollaborationAction{ActionID: id, State: "completed", Result: encoded, Revision: 1}
	if _, err = tx.Exec("INSERT INTO collaboration_actions(run_id,action_id,fingerprint,action_json,state,result_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", run, id, fp, string(raw), receipt.State, string(encoded), now, now); err != nil {
		return receipt, false, err
	}
	if err = tx.Commit(); err != nil {
		return receipt, false, err
	}
	return receipt, true, nil
}

func (s *Store) CollaborationAction(u User, run, id string) (CollaborationAction, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return CollaborationAction{}, err
	}
	var a CollaborationAction
	var raw string
	err := s.db.QueryRow("SELECT action_id,state,result_json,revision FROM collaboration_actions WHERE run_id=? AND action_id=?", run, id).Scan(&a.ActionID, &a.State, &raw, &a.Revision)
	a.Result = json.RawMessage(raw)
	return a, err
}

func (s *Store) AbandonCollaborationAction(u User, run, id string) error {
	if !collaboration.ValidID(id) {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = collaborationRunTx(tx, u, run); err != nil {
		return err
	}
	var state string
	err = tx.QueryRow("SELECT state FROM collaboration_actions WHERE run_id=? AND action_id=?", run, id).Scan(&state)
	if err == nil {
		if state == "abandoned" {
			return nil
		}
		return ErrChatConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := usageTimestamp(time.Now())
	if _, err = tx.Exec("INSERT INTO collaboration_actions(run_id,action_id,fingerprint,action_json,state,created_at,updated_at) VALUES(?,?,?,'null','abandoned',?,?)", run, id, "", now, now); err != nil {
		return err
	}
	return tx.Commit()
}
