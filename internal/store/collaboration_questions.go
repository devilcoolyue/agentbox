package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"agentbox/internal/collaboration"
)

type collaborationQuestion struct {
	ID, Kind, Parent, State    string
	ConfigVersion, PlanVersion int64
}

func collaborationQuestionTx(tx *sql.Tx, r *CollaborationRun, id string) (collaborationQuestion, error) {
	q := collaborationQuestion{ID: id}
	err := tx.QueryRow("SELECT origin_kind,parent_question,config_version,plan_version,state FROM collaboration_questions WHERE run_id=? AND message_id=?", r.ID, id).Scan(&q.Kind, &q.Parent, &q.ConfigVersion, &q.PlanVersion, &q.State)
	if err != nil {
		return q, err
	}
	if q.ConfigVersion != r.ConfigVersion || q.PlanVersion != r.PlanVersion || q.State != "open" {
		return q, ErrChatConflict
	}
	return q, nil
}

func openCollaborationQuestionTx(tx *sql.Tx, r *CollaborationRun, id, kind, parent string) error {
	if parent != "" {
		q, err := collaborationQuestionTx(tx, r, parent)
		if err != nil {
			return err
		}
		if q.Kind != "execute" && q.Kind != "repair" {
			return ErrChatConflict
		}
	}
	_, err := tx.Exec("INSERT INTO collaboration_questions(run_id,message_id,origin_kind,parent_question,config_version,plan_version) VALUES(?,?,?,?,?,?)", r.ID, id, kind, parent, r.ConfigVersion, r.PlanVersion)
	return err
}

func answerCollaborationTx(tx *sql.Tx, r *CollaborationRun, payload json.RawMessage) error {
	var answer collaboration.Answer
	if collaboration.Decode(payload, &answer, collaboration.MaxHandoffBytes) != nil || !collaboration.ValidID(answer.QuestionID) {
		return ErrChatInvalid
	}
	// Reuse the content contract, but the trusted sender is always the user.
	encoded, _ := json.Marshal(collaboration.Handoff{Version: 1, Type: "answer", Payload: payload})
	if _, err := collaboration.ParseHandoff(encoded, collaboration.Coordinator); err != nil {
		return ErrChatInvalid
	}
	if r.State != CollabWaitingUser {
		return collaboration.ErrState
	}
	if err := collaborationStoppedTx(tx, r.ID); err != nil {
		return err
	}
	var pending, snapshot string
	if err := tx.QueryRow("SELECT pending_question,snapshot_id FROM collaborations WHERE id=?", r.ID).Scan(&pending, &snapshot); err != nil {
		return err
	}
	if answer.QuestionID != pending {
		return ErrChatConflict
	}
	question, err := collaborationQuestionTx(tx, r, pending)
	if err != nil {
		return err
	}
	switch question.Kind {
	case "plan":
		r.State, r.Phase = CollabPlanning, CollabPlanning
	case "review":
		r.State, r.Phase = CollabReviewing, CollabReviewing
	case "execute", "repair":
		if !r.Request.Workflow.IsDirect() {
			return collaboration.ErrState
		}
		r.State, r.Phase = CollabExecuting, CollabExecuting
	case "clarify":
		if _, err = collaborationQuestionTx(tx, r, question.Parent); err != nil {
			return err
		}
		r.State = CollabWaitingAnswer
	default:
		return collaboration.ErrState
	}
	if _, err = tx.Exec("UPDATE collaboration_questions SET state='answered' WHERE run_id=? AND message_id=?", r.ID, pending); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaborations SET pending_question=? WHERE id=?", question.Parent, r.ID); err != nil {
		return err
	}
	recipient := "coordinator"
	if r.Request.Workflow.IsDirect() {
		recipient = "executor"
	}
	if err = appendCollaborationMessageTx(tx, r, NewCollaborationID(), "", "user", recipient, "answer", pending, payload); err != nil {
		return err
	}
	r.WaitReason = ""
	if r.StepsUsed >= r.MaxSteps {
		r.NextKind = question.Kind
		pauseCollaboration(r, "step_limit")
		return nil
	}
	_, err = queueCollaborationStepTx(tx, r, question.Kind, snapshot)
	return err
}

func appendCollaborationMessageTx(tx *sql.Tx, r *CollaborationRun, id, step, sender, recipient, kind, reply string, body json.RawMessage) error {
	now := usageTimestamp(time.Now())
	_, err := tx.Exec("INSERT INTO collaboration_messages(run_id,seq,id,step_id,sender_role,recipient_role,type,config_version,plan_version,reply_to,body_json,created_at) SELECT ?,COALESCE(MAX(seq),0)+1,?,?,?,?,?,?,?,?,?,? FROM collaboration_messages WHERE run_id=?", r.ID, id, step, sender, recipient, kind, r.ConfigVersion, r.PlanVersion, reply, string(body), now, r.ID)
	if err != nil {
		return err
	}
	return collaborationEventTx(tx, r.ID, "message", map[string]any{"id": id, "step_id": step, "sender_role": sender, "recipient_role": recipient, "type": kind, "reply_to": reply, "body": body, "config_version": r.ConfigVersion, "plan_version": r.PlanVersion})
}
