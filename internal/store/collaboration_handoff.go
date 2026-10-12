package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"agentbox/internal/collaboration"
)

func NewCollaborationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func collaborationMessagesTx(tx *sql.Tx, run string) ([]collaboration.ContextMessage, error) {
	rows, err := tx.Query("SELECT id,sender_role,type,body_json FROM collaboration_messages WHERE run_id=? ORDER BY seq", run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []collaboration.ContextMessage{}
	size := 0
	for rows.Next() {
		var m collaboration.ContextMessage
		var raw string
		if err = rows.Scan(&m.ID, &m.Sender, &m.Type, &raw); err != nil {
			return nil, err
		}
		size += len(raw)
		if size > collaboration.MaxInputBytes {
			return nil, collaboration.ErrInvalid
		}
		m.Body = json.RawMessage(raw)
		out = append(out, m)
	}
	return out, rows.Err()
}

func queueCollaborationStepTx(tx *sql.Tx, r *CollaborationRun, kind, snapshot string) (CollaborationStep, error) {
	role := collaboration.Coordinator
	if kind == "execute" || kind == "repair" {
		role = collaboration.Executor
	}
	var generation int
	if err := tx.QueryRow("SELECT generation FROM collaboration_participants WHERE run_id=? AND role=? AND config_version=? AND state='active' ORDER BY generation DESC LIMIT 1", r.ID, string(role), r.ConfigVersion).Scan(&generation); err != nil {
		return CollaborationStep{}, err
	}
	input, err := collaborationStepPromptTx(tx, r, role, kind, snapshot, generation, false)
	if errors.Is(err, errCollaborationContextLimit) {
		r.NextKind = kind
		pauseCollaboration(r, "context_limit")
		return CollaborationStep{}, nil
	}
	if err != nil {
		return CollaborationStep{}, err
	}
	return addCollaborationStepTx(tx, r, kind, string(role), generation, input)
}

func collaborationStepPromptTx(tx *sql.Tx, r *CollaborationRun, role collaboration.Role, kind, snapshot string, generation int, reformat bool) (CollaborationStepInput, error) {
	if r.Request == nil {
		return CollaborationStepInput{}, ErrChatGone
	}
	cfg, err := collaborationConfigTx(tx, r)
	if err != nil {
		return CollaborationStepInput{}, err
	}
	var pending string
	if err = tx.QueryRow("SELECT pending_question FROM collaborations WHERE id=?", r.ID).Scan(&pending); err != nil {
		return CollaborationStepInput{}, err
	}
	options := collaboration.PromptOptions{Workflow: cfg.Workflow, VerificationCommand: cfg.VerificationCommand}
	if kind == "review" {
		options.ReviewEvidence, err = collaborationReviewEvidenceTx(tx, r, snapshot)
		if err != nil {
			return CollaborationStepInput{}, err
		}
	}
	var verification []collaboration.ContextMessage
	if kind == "review" || (cfg.Workflow.IsDirect() && kind == "repair" && !reformat) {
		v, e := collaborationVerificationTx(tx, r.ID, snapshot, r.ConfigVersion)
		previous := false
		if kind == "repair" && errors.Is(e, sql.ErrNoRows) {
			// An executor question publishes its current workspace too. Its
			// continuation still needs the failure that caused this repair,
			// explicitly attributed to the earlier snapshot, never to this one.
			var prior string
			e = tx.QueryRow("SELECT snapshot_id FROM collaboration_verifications WHERE run_id=? AND config_version=? AND state IN ('completed','failed','not_requested') ORDER BY updated_at DESC,snapshot_id DESC LIMIT 1", r.ID, r.ConfigVersion).Scan(&prior)
			if e == nil {
				v, e = collaborationVerificationTx(tx, r.ID, prior, r.ConfigVersion)
				previous = true
			}
		}
		if e != nil {
			return CollaborationStepInput{}, e
		}
		if v.State != "completed" && v.State != "failed" && v.State != "not_requested" {
			return CollaborationStepInput{}, collaboration.ErrState
		}
		raw, e := json.Marshal(v)
		if e != nil {
			return CollaborationStepInput{}, e
		}
		messageType := "platform_verification"
		if previous {
			messageType = "previous_platform_verification"
		}
		verification = append(verification, collaboration.ContextMessage{ID: v.HelperID, Sender: "system", Type: messageType, Body: raw})
	}
	var messages []collaboration.ContextMessage
	var boundary *CollaborationContextBoundary
	if cfg.Workflow == nil {
		messages, err = collaborationMessagesTx(tx, r.ID)
	} else {
		// Count encoded bytes: escaped tool output or user requirements can be
		// several times larger than their raw strings. Keep hard evidence intact.
		encodedOptions, _ := json.Marshal(options)
		encodedGoal, _ := json.Marshal(r.Request.PromptGoal())
		encodedVerification, _ := json.Marshal(verification)
		budget := collaboration.MaxInputBytes - len(encodedGoal) - len(encodedOptions) - len(encodedVerification) - (16 << 10)
		if budget > 512<<10 {
			budget = 512 << 10
		}
		messages, boundary, err = collaborationContextTx(tx, r, string(role), generation, budget)
	}
	if err != nil {
		return CollaborationStepInput{}, err
	}
	messages = append(messages, verification...)
	var prompt string
	if cfg.Workflow == nil {
		prompt, err = collaboration.BuildPrompt(role, kind, r.Request.PromptGoal(), snapshot, r.PlanVersion, messages, pending)
	} else {
		prompt, err = collaboration.BuildPromptWithOptions(role, kind, r.Request.PromptGoal(), snapshot, r.PlanVersion, messages, options, pending)
	}
	if err != nil {
		return CollaborationStepInput{}, err
	}
	return CollaborationStepInput{Text: prompt, SnapshotID: snapshot, Context: boundary}, nil
}

// CollaborationCompletion is supplied by the common turn runner ONLY after
// provider success, transcript fsync, original usage settlement and container
// stop have succeeded. It never repairs accounting or executes a second turn.
type CollaborationCompletion struct {
	Final          []byte
	SnapshotID     string
	HistorySynced  bool
	UsageSettled   bool
	ProcessStopped bool
}

func (s *Store) CompleteCollaborationStep(u User, run, id string, revision int64, in CollaborationCompletion) (CollaborationRun, error) {
	if !in.HistorySynced || !in.UsageSettled || !in.ProcessStopped {
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
	if st.Revision != revision || st.State != "finalizing" || st.ConfigVersion != r.ConfigVersion || st.PlanVersion != r.PlanVersion {
		return r, ErrChatConflict
	}
	if r.State != CollabPlanning && r.State != CollabExecuting && r.State != CollabWaitingAnswer && r.State != CollabReviewing && r.State != CollabCancelling {
		return r, ErrChatConflict
	}
	if r.State == CollabCancelling {
		// Provider success may race cancellation. Preserve the successful
		// step, but do not demand a new handoff/snapshot or schedule more work.
		st.State = "completed"
		st.Revision++
		st.UpdatedAt = usageTimestamp(time.Now())
		if _, err = tx.Exec("UPDATE collaboration_steps SET ended_at=?,state=?,revision=?,updated_at=? WHERE id=?", st.UpdatedAt, st.State, st.Revision, st.UpdatedAt, id); err != nil {
			return r, err
		}
		if _, err = tx.Exec("UPDATE collaboration_participants SET stopped_epoch=process_epoch WHERE run_id=? AND role=? AND generation=?", run, st.Role, st.ParticipantGeneration); err != nil {
			return r, err
		}
		r.State, r.WaitReason, r.NextKind = CollabCancelled, "user_cancelled", ""
		if err = collaborationEventTx(tx, run, "step", st); err != nil {
			return r, err
		}
		if err = saveCollaborationTx(tx, &r); err != nil {
			return r, err
		}
		return r, tx.Commit()
	}
	originalKind := st.Kind
	if st.Kind == "handoff_repair" {
		var raw string
		var input CollaborationStepInput
		if err = tx.QueryRow("SELECT input_json FROM collaboration_steps WHERE id=?", st.ID).Scan(&raw); err != nil {
			return r, err
		}
		if err = json.Unmarshal([]byte(raw), &input); err != nil {
			return r, err
		}
		originalKind = input.RepairKind
	}
	h, parseErr := collaboration.ParseHandoff(in.Final, collaboration.Role(st.Role))
	allowed := map[string]map[string]bool{
		"plan": {"plan": true, "question": true}, "execute": {"result": true, "question": true}, "repair": {"result": true, "question": true}, "clarify": {"answer": true, "question": true}, "review": {"review": true, "question": true},
	}
	if !allowed[originalKind][h.Type] {
		parseErr = collaboration.ErrInvalid
	}
	var pause, repairs int
	var snapshot, pending string
	if err = tx.QueryRow("SELECT pause_requested,repairs_used,snapshot_id,pending_question FROM collaborations WHERE id=?", run).Scan(&pause, &repairs, &snapshot, &pending); err != nil {
		return r, err
	}
	// A submitted snapshot must be an already published artifact of THIS run.
	if in.SnapshotID != "" {
		var n int
		if err = tx.QueryRow("SELECT count(*) FROM collaboration_artifacts WHERE id=? AND run_id=? AND state='ready' AND kind IN ('baseline','snapshot')", in.SnapshotID, run).Scan(&n); err != nil {
			return r, err
		}
		if n != 1 {
			return r, ErrChatInvalid
		}
		snapshot = in.SnapshotID
	}
	next := ""
	r.NextKind = ""
	messageID := NewCollaborationID()
	replyTo := ""
	if parseErr == nil {
		switch h.Type {
		case "plan":
			r.PlanVersion++
			r.State, r.Phase = CollabAwaitingPlan, CollabExecuting
			var raw string
			var cfg CollaborationConfig
			if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", run, r.ConfigVersion).Scan(&raw); err != nil {
				return r, err
			}
			if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
				return r, err
			}
			if cfg.AutoExecute {
				r.State = CollabExecuting
				next = "execute"
			}
		case "question":
			parent := ""
			if originalKind == "clarify" {
				parent = pending
			}
			if err = openCollaborationQuestionTx(tx, &r, messageID, originalKind, parent); err != nil {
				return r, err
			}
			pending = messageID
			if st.Role == "executor" && !r.Request.Workflow.IsDirect() {
				r.State = CollabWaitingAnswer
				next = "clarify"
			} else {
				r.State = CollabWaitingUser
			}
		case "answer":
			var a collaboration.Answer
			_ = json.Unmarshal(h.Payload, &a)
			if a.QuestionID != pending || pending == "" {
				parseErr = collaboration.ErrInvalid
				break
			}
			q, e := collaborationQuestionTx(tx, &r, pending)
			if e != nil || (q.Kind != "execute" && q.Kind != "repair") {
				parseErr = collaboration.ErrInvalid
				break
			}
			if _, err = tx.Exec("UPDATE collaboration_questions SET state='answered' WHERE run_id=? AND message_id=?", run, pending); err != nil {
				return r, err
			}
			replyTo = pending
			pending = ""
			r.State, r.Phase = CollabExecuting, CollabExecuting
			next = q.Kind
		case "result":
			if snapshot == "" || in.SnapshotID == "" {
				return r, ErrChatInvalid
			}
			r.State, r.Phase = CollabReviewing, CollabReviewing
			if r.Request.Workflow.IsDirect() {
				r.State, r.Phase = CollabExecuting, CollabExecuting
			}
			pauseCollaboration(&r, "verification_required")
		case "review":
			var review collaboration.Review
			_ = json.Unmarshal(h.Payload, &review)
			if review.SnapshotID != snapshot || review.SnapshotID != st.SnapshotID || review.PlanVersion != r.PlanVersion {
				parseErr = collaboration.ErrInvalid
				break
			}
			switch review.Decision {
			case "approve":
				// Verification and delivery publication are a separate durable
				// gate. A model's approval alone cannot complete the task.
				var verified string
				if err = tx.QueryRow("SELECT state FROM collaboration_verifications WHERE run_id=? AND snapshot_id=? AND config_version=?", run, snapshot, r.ConfigVersion).Scan(&verified); err != nil {
					return r, err
				}
				if verified != "completed" && verified != "not_requested" {
					pauseCollaboration(&r, "verification_failed")
				} else {
					pauseCollaboration(&r, "delivery_required")
				}
			case "revise":
				if repairs >= r.MaxRepairs {
					r.State, r.Phase = CollabExecuting, CollabExecuting
					r.NextKind = "repair"
					pauseCollaboration(&r, "repair_limit")
				} else {
					repairs++
					r.State, r.Phase = CollabExecuting, CollabExecuting
					next = "repair"
				}
			case "needs_user":
				if err = openCollaborationQuestionTx(tx, &r, messageID, "review", ""); err != nil {
					return r, err
				}
				pending = messageID
				r.State = CollabWaitingUser
			}
		}
	}
	if parseErr != nil {
		pauseCollaboration(&r, "handoff_invalid")
		next = ""
	} else {
		recipient := "executor"
		if st.Role == "executor" {
			recipient = "coordinator"
			if r.Request.Workflow.IsDirect() {
				recipient = "user"
			}
		} else if h.Type == "question" || r.State == CollabWaitingUser {
			recipient = "user"
		}
		err = appendCollaborationMessageTx(tx, &r, messageID, id, st.Role, recipient, h.Type, replyTo, h.Payload)
		if err != nil {
			return r, err
		}
	}
	st.State = "completed"
	st.Revision++
	st.UpdatedAt = usageTimestamp(time.Now())
	if _, err = tx.Exec("UPDATE collaboration_steps SET ended_at=?,state=?,revision=?,updated_at=? WHERE id=?", st.UpdatedAt, st.State, st.Revision, st.UpdatedAt, id); err != nil {
		return r, err
	}
	if pause != 0 && r.State == CollabPaused && (r.WaitReason == "verification_required" || r.WaitReason == "delivery_required") {
		r.NextKind = r.WaitReason
		r.WaitReason = "user_paused"
	}
	if pause != 0 && r.State != CollabPaused {
		r.NextKind = next
		pauseCollaboration(&r, "user_paused")
		next = ""
	}
	// Cancellation wins even if the provider had already finished successfully.
	var persistedState string
	if err = tx.QueryRow("SELECT state FROM collaborations WHERE id=?", run).Scan(&persistedState); err != nil {
		return r, err
	}
	if persistedState == CollabCancelling {
		r.State, r.WaitReason = CollabCancelled, "user_cancelled"
		next = ""
	}
	if next != "" && r.StepsUsed >= r.MaxSteps {
		r.NextKind = next
		pauseCollaboration(&r, "step_limit")
		next = ""
	}
	if _, err = tx.Exec("UPDATE collaborations SET snapshot_id=?,pending_question=?,repairs_used=?,pause_requested=0 WHERE id=?", snapshot, pending, repairs, run); err != nil {
		return r, err
	}
	if _, err = tx.Exec("UPDATE collaboration_participants SET stopped_epoch=process_epoch WHERE run_id=? AND role=? AND generation=?", run, st.Role, st.ParticipantGeneration); err != nil {
		return r, err
	}
	if parseErr == nil {
		if err = acknowledgeCollaborationContextTx(tx, &r, st); err != nil {
			return r, err
		}
	}
	if next != "" {
		if _, err = queueCollaborationStepTx(tx, &r, next, snapshot); err != nil {
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
