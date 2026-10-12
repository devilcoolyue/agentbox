package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"agentbox/internal/collaboration"
)

// FailCollaborationStep fences a failed attempt; it never retries it. Even a
// stopped CLI may have changed files or produced unsettled usage. The caller
// records the original usage before invoking this method where possible.
func (s *Store) FailCollaborationStep(u User, run, id string, revision int64, code string, processStopped bool) (CollaborationRun, error) {
	switch code {
	case "execution_failed", "history_write_failed", "usage_write_failed", "resume_failed", "authorization_revoked", "operation_cancelled", "container_stop_failed":
	default:
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
	st, err := scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE id=? AND run_id=?", id, run))
	if err != nil {
		return r, err
	}
	if st.Revision != revision || st.ConfigVersion != r.ConfigVersion || (st.State != "starting" && st.State != "running" && st.State != "finalizing") {
		return r, ErrChatConflict
	}
	if r.State == CollabDeleted || r.State == CollabCancelled || r.State == CollabCompleted || r.State == CollabUncertain {
		return r, ErrChatConflict
	}
	beforeModel := st.State == "starting"
	st.State = "uncertain"
	st.ErrorCode = code
	st.Revision++
	st.UpdatedAt = usageTimestamp(time.Now())
	r.ResumeState = r.State
	r.WaitReason = code
	r.NextKind = ""
	if beforeModel && processStopped {
		st.State = "failed"
		r.NextKind = st.Kind
		if r.State == CollabCancelling {
			r.State = CollabCancelled
			r.NextKind = ""
		} else {
			r.State = CollabPaused
		}
	} else {
		r.State = CollabUncertain
	}
	if _, err = tx.Exec("UPDATE collaboration_steps SET ended_at=?,state=?,error_code=?,revision=?,updated_at=? WHERE id=?", st.UpdatedAt, st.State, st.ErrorCode, st.Revision, st.UpdatedAt, id); err != nil {
		return r, err
	}
	if processStopped {
		if _, err = tx.Exec("UPDATE collaboration_participants SET stopped_epoch=process_epoch WHERE run_id=? AND role=? AND generation=?", run, st.Role, st.ParticipantGeneration); err != nil {
			return r, err
		}
	}
	if err = collaborationEventTx(tx, run, "step", st); err != nil {
		return r, err
	}
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

func reviewCollaborationTx(tx *sql.Tx, r *CollaborationRun, payload json.RawMessage) error {
	var input struct {
		StepID       string `json:"step_id"`
		StepRevision int64  `json:"step_revision"`
		Acknowledged bool   `json:"acknowledged"`
	}
	if collaboration.Decode(payload, &input, 4096) != nil || !input.Acknowledged || input.StepID == "" || input.StepRevision < 1 {
		return ErrChatInvalid
	}
	if r.State != CollabUncertain {
		return collaboration.ErrState
	}
	if err := collaborationStoppedTx(tx, r.ID); err != nil {
		return err
	}
	step, err := scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE id=? AND run_id=?", input.StepID, r.ID))
	if err != nil {
		return err
	}
	if step.State != "uncertain" || step.Revision != input.StepRevision || step.ConfigVersion != r.ConfigVersion {
		return ErrChatConflict
	}
	// Keep an explicit reviewed state: this is not proof of successful work,
	// and the original failed/unknown accounting is not silently repaired.
	step.State = "reviewed"
	step.Revision++
	step.UpdatedAt = usageTimestamp(time.Now())
	if _, err = tx.Exec("UPDATE collaboration_steps SET state=?,revision=?,updated_at=? WHERE id=?", step.State, step.Revision, step.UpdatedAt, step.ID); err != nil {
		return err
	}
	r.State, r.WaitReason = CollabPaused, "result_reviewed"
	// After uncertain side effects the user must create a new configuration
	// generation and planning step, not rerun the original input implicitly.
	r.NextKind = ""
	r.ResumeState = ""
	if err = collaborationEventTx(tx, r.ID, "step", step); err != nil {
		return err
	}
	return nil
}
