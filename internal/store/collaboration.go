package store

import (
	"agentbox/internal/collaboration"
	"agentbox/internal/decision"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	CollabAccepted      = "accepted"
	CollabPreparing     = "preparing"
	CollabPlanning      = "planning"
	CollabAwaitingPlan  = "awaiting_plan"
	CollabExecuting     = "executing"
	CollabWaitingAnswer = "waiting_answer"
	CollabWaitingUser   = "waiting_user"
	CollabReviewing     = "reviewing"
	CollabPaused        = "paused"
	CollabCancelling    = "cancelling"
	CollabCancelled     = "cancelled"
	CollabCompleted     = "completed"
	CollabFailed        = "failed"
	CollabUncertain     = "uncertain"
	CollabDeleted       = "deleted"
)

type CollaborationBinding = collaboration.Binding
type CollaborationRoles = collaboration.Roles
type CollaborationWorkflow = collaboration.Workflow

type CollaborationConfig struct {
	Workflow *CollaborationWorkflow `json:"workflow,omitempty"`
	CollaborationLimits
	VerificationCommand string             `json:"verification_command"`
	Roles               CollaborationRoles `json:"roles"`
	AutoExecute         bool               `json:"auto_execute"`
	MaxRepairs          int                `json:"max_repairs"`
	MaxSteps            int                `json:"max_steps"`
	BudgetMicroUSD      int64              `json:"budget_micro_usd"`
}
type CollaborationRun struct {
	CleanupPending  bool                      `json:"cleanup_pending"`
	ID              string                    `json:"id"`
	User            string                    `json:"user"`
	SourceSessionID string                    `json:"source_session_id"`
	PendingQuestion string                    `json:"pending_question,omitempty"`
	Scope           string                    `json:"scope"`
	RequestID       string                    `json:"request_id"`
	Request         *CollaborationCreateInput `json:"request,omitempty"`
	State           string                    `json:"state"`
	Phase           string                    `json:"phase"`
	WaitReason      string                    `json:"wait_reason,omitempty"`
	Revision        int64                     `json:"revision"`
	ConfigVersion   int64                     `json:"config_version"`
	PlanVersion     int64                     `json:"plan_version"`
	MaxRepairs      int                       `json:"max_repairs"`
	MaxSteps        int                       `json:"max_steps"`
	StepsUsed       int                       `json:"steps_used"`
	BudgetMicroUSD  int64                     `json:"budget_micro_usd"`
	CreatedAt       string                    `json:"created_at"`
	UpdatedAt       string                    `json:"updated_at"`
	ResumeState     string                    `json:"resume_state,omitempty"`
	NextKind        string                    `json:"next_kind,omitempty"`
}
type CollaborationThreadContext struct {
	ThreadID  string `json:"thread_id"`
	Anchor    int    `json:"anchor"`
	Include   bool   `json:"include"`
	SHA256    string `json:"sha256"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

type CollaborationCreateInput struct {
	Workflow *CollaborationWorkflow      `json:"workflow,omitempty"`
	Decision *decision.Intent            `json:"decision,omitempty"`
	Context  *CollaborationThreadContext `json:"context,omitempty"`
	CollaborationLimits
	VerificationCommand string             `json:"verification_command"`
	Scope               string             `json:"scope"`
	SourceSessionID     string             `json:"source_session_id"`
	Text                string             `json:"text"`
	Repository          string             `json:"repository"`
	Baseline            string             `json:"baseline"`
	Roles               CollaborationRoles `json:"roles"`
	AutoExecute         bool               `json:"auto_execute"`
	MaxRepairs          int                `json:"max_repairs"`
	MaxSteps            int                `json:"max_steps"`
	BudgetMicroUSD      int64              `json:"budget_micro_usd"`
}
type CollaborationActionInput struct {
	Scope            string          `json:"scope"`
	ExpectedRevision int64           `json:"expected_revision"`
	Action           string          `json:"action"`
	Payload          json.RawMessage `json:"payload,omitempty"`
}
type CollaborationAction struct {
	ActionID string          `json:"action_id"`
	State    string          `json:"state"`
	Result   json.RawMessage `json:"result"`
	Revision int64           `json:"revision"`
}
type CollaborationStep struct {
	ID                    string `json:"id"`
	RunID                 string `json:"run_id"`
	Kind                  string `json:"kind"`
	Role                  string `json:"role"`
	ParticipantGeneration int    `json:"participant_generation"`
	ConfigVersion         int64  `json:"config_version"`
	PlanVersion           int64  `json:"plan_version"`
	Attempt               int    `json:"attempt"`
	TurnID                string `json:"turn_id"`
	State                 string `json:"state"`
	ErrorCode             string `json:"error_code,omitempty"`
	Revision              int64  `json:"revision"`
	SnapshotID            string `json:"snapshot_id,omitempty"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}
type CollaborationMessage struct {
	RunID         string          `json:"run_id"`
	Seq           int64           `json:"seq"`
	ID            string          `json:"id"`
	StepID        string          `json:"step_id"`
	SenderRole    string          `json:"sender_role"`
	RecipientRole string          `json:"recipient_role,omitempty"`
	Type          string          `json:"type"`
	ConfigVersion int64           `json:"config_version"`
	PlanVersion   int64           `json:"plan_version"`
	ReplyTo       string          `json:"reply_to,omitempty"`
	Body          json.RawMessage `json:"body"`
	CreatedAt     string          `json:"created_at"`
}

func collaborationFingerprint(v any) (string, []byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw, nil
}
func scanCollaboration(row interface{ Scan(...any) error }) (CollaborationRun, error) {
	var r CollaborationRun
	var raw string
	err := row.Scan(&r.ID, &r.User, &r.SourceSessionID, &r.RequestID, &raw, &r.State, &r.Phase, &r.WaitReason, &r.Revision, &r.ConfigVersion, &r.PlanVersion, &r.MaxRepairs, &r.MaxSteps, &r.StepsUsed, &r.BudgetMicroUSD, &r.CreatedAt, &r.UpdatedAt, &r.ResumeState, &r.NextKind, &r.Scope, &r.PendingQuestion, &r.CleanupPending)
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &r.Request)
	}
	return r, err
}

const collaborationCols = "id,user,source_session_id,request_id,request_json,state,phase,wait_reason,revision,config_version,plan_version,max_repairs,max_steps,steps_used,budget_micro_usd,created_at,updated_at,resume_state,next_kind,current_scope,pending_question,cleanup_pending"

func (s *Store) Collaboration(u User, id string) (CollaborationRun, error) {
	if !validChatKey(id) {
		return CollaborationRun{}, ErrChatInvalid
	}
	return scanCollaboration(s.db.QueryRow("SELECT "+collaborationCols+" FROM collaborations WHERE id=? AND user=? AND user_created_at=? AND EXISTS(SELECT 1 FROM users WHERE name=? AND created_at=?)", id, u.Name, ownerEpoch(u), u.Name, ownerEpoch(u)))
}
func (s *Store) ListCollaborations(u User) ([]CollaborationRun, error) {
	rows, err := s.db.Query("SELECT "+collaborationCols+" FROM collaborations WHERE user=? AND user_created_at=? AND (state!='deleted' OR cleanup_pending=1) AND EXISTS(SELECT 1 FROM users WHERE name=? AND created_at=?) ORDER BY updated_at DESC LIMIT 100", u.Name, ownerEpoch(u), u.Name, ownerEpoch(u))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaborationRun{}
	for rows.Next() {
		r, e := scanCollaboration(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CreateCollaboration(u User, id string, in CollaborationCreateInput, cfg CollaborationConfig) (CollaborationRun, bool, error) {
	return s.createCollaboration(u, id, in, cfg, nil)
}

// CreateCollaborationWithDecision freezes a server-owned policy snapshot in the
// same transaction as the requested intent. The snapshot is not its fingerprint.
func (s *Store) CreateCollaborationWithDecision(u User, id string, in CollaborationCreateInput, cfg CollaborationConfig, snapshot decision.Snapshot) (CollaborationRun, bool, error) {
	return s.createCollaboration(u, id, in, cfg, &snapshot)
}

func (s *Store) createCollaboration(u User, id string, in CollaborationCreateInput, cfg CollaborationConfig, snapshot *decision.Snapshot) (CollaborationRun, bool, error) {
	if !collaboration.ValidID(id) || strings.TrimSpace(in.Text) == "" || len(in.Text) > collaboration.MaxInputBytes || in.Roles.Validate() != nil || in.Scope == "" || len(in.Scope) > 128 || !validChatKey(in.SourceSessionID) || (len(in.Baseline) != 40 && len(in.Baseline) != 64) {
		return CollaborationRun{}, false, ErrChatInvalid
	}
	if _, err := hex.DecodeString(in.Baseline); err != nil {
		return CollaborationRun{}, false, ErrChatInvalid
	}
	if !sameCollaborationWorkflow(in.Workflow, cfg.Workflow) || (in.Workflow != nil && in.Workflow.Validate() != nil) || !cfg.CollaborationLimits.Valid() || cfg.CollaborationLimits != in.CollaborationLimits || cfg.Roles != in.Roles || cfg.AutoExecute != in.AutoExecute || cfg.MaxRepairs != in.MaxRepairs || cfg.MaxSteps != in.MaxSteps || cfg.BudgetMicroUSD != in.BudgetMicroUSD || cfg.VerificationCommand != in.VerificationCommand || len(cfg.VerificationCommand) > 8192 || strings.ContainsRune(cfg.VerificationCommand, 0) {
		return CollaborationRun{}, false, ErrChatInvalid
	}
	if in.Context != nil && (!validChatKey(in.Context.ThreadID) || in.Context.Anchor < 0 || len(in.Context.Text) > 24000 || len(in.Context.SHA256) != 64) {
		return CollaborationRun{}, false, ErrChatInvalid
	}
	fp, raw, err := collaborationFingerprint(in)
	if err != nil {
		return CollaborationRun{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, false, err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return CollaborationRun{}, false, err
	}
	r, err := scanCollaboration(tx.QueryRow("SELECT "+collaborationCols+" FROM collaborations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id))
	if err == nil {
		if r.State == CollabDeleted {
			return r, false, ErrChatGone
		}
		old, _, e := collaborationFingerprint(existingRequest(r))
		if e == nil && old == fp {
			return r, false, nil
		}
		return r, false, ErrChatConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CollaborationRun{}, false, err
	}
	// Replay above uses only the original request, even if policy changed since.
	if (in.Decision == nil) != (snapshot == nil) {
		return CollaborationRun{}, false, ErrChatInvalid
	}
	if snapshot != nil {
		// Normalize omitted metadata only before the atomic initial acceptance.
		// A supplied digest must match, and persisted snapshots are never rebuilt.
		copy := *snapshot
		if copy.TemplateVersion == "" {
			copy.TemplateVersion = decision.TemplateVersion
		}
		if copy.CandidateVersion == "" {
			copy.CandidateVersion, err = decisionCandidateDigest(&copy)
			if err != nil {
				return CollaborationRun{}, false, err
			}
		}
		snapshot = &copy
		if err = validateCollaborationDecision(in, *snapshot); err != nil {
			return CollaborationRun{}, false, err
		}
	}
	if err = checkChatSession(tx, u, in.SourceSessionID); err != nil {
		return CollaborationRun{}, false, err
	}
	if cfg.MaxRepairs < 0 || cfg.MaxRepairs > 20 || cfg.MaxSteps < 1 || cfg.MaxSteps > 100 || cfg.BudgetMicroUSD < 0 {
		return CollaborationRun{}, false, ErrChatInvalid
	}
	if in.Context != nil {
		var active int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM collaboration_threads t JOIN collaborations c ON c.id=t.run_id WHERE t.session_id=? AND t.thread_id=? AND c.state NOT IN ('completed','cancelled','failed','deleted')`, in.SourceSessionID, in.Context.ThreadID).Scan(&active); err != nil {
			return CollaborationRun{}, false, err
		}
		if active != 0 {
			return CollaborationRun{}, false, ErrChatConflict
		}
	}
	now := usageTimestamp(time.Now())
	identity, _ := json.Marshal([]string{u.Name, ownerEpoch(u), id})
	sum := sha256.Sum256(identity)
	runID := hex.EncodeToString(sum[:16])
	r = CollaborationRun{ID: runID, User: u.Name, SourceSessionID: in.SourceSessionID, RequestID: id, Scope: in.Scope, Request: &in, State: CollabAccepted, Phase: CollabPreparing, Revision: 1, ConfigVersion: 1, MaxRepairs: cfg.MaxRepairs, MaxSteps: cfg.MaxSteps, BudgetMicroUSD: cfg.BudgetMicroUSD, CreatedAt: now, UpdatedAt: now}
	if _, err = tx.Exec("INSERT INTO collaborations("+collaborationCols+",user_created_at,fingerprint) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", runID, u.Name, in.SourceSessionID, id, string(raw), r.State, r.Phase, "", 1, 1, 0, r.MaxRepairs, r.MaxSteps, 0, r.BudgetMicroUSD, now, now, "", "", in.Scope, "", 0, ownerEpoch(u), fp); err != nil {
		return CollaborationRun{}, false, err
	}
	if in.Context != nil {
		if _, err = tx.Exec("INSERT INTO collaboration_threads(run_id,session_id,thread_id,anchor) VALUES(?,?,?,?)", runID, in.SourceSessionID, in.Context.ThreadID, in.Context.Anchor); err != nil {
			return CollaborationRun{}, false, err
		}
	}
	cfgRaw, _ := json.Marshal(cfg)
	if _, err = tx.Exec("INSERT INTO collaboration_configs(run_id,version,config_json,created_at) VALUES(?,?,?,?)", runID, 1, string(cfgRaw), now); err != nil {
		return CollaborationRun{}, false, err
	}
	if _, err = tx.Exec("INSERT INTO collaboration_events(run_id,seq,type,body_json,created_at) VALUES(?,?,?,?,?)", runID, 1, "created", string(raw), now); err != nil {
		return CollaborationRun{}, false, err
	}
	if snapshot != nil {
		snapshotRaw, e := json.Marshal(snapshot)
		if e != nil {
			return CollaborationRun{}, false, e
		}
		if _, err = tx.Exec("INSERT INTO collaboration_decisions(run_id,state,snapshot_json,created_at,updated_at) VALUES(?,'pending',?,?,?)", runID, string(snapshotRaw), now, now); err != nil {
			return CollaborationRun{}, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return CollaborationRun{}, false, err
	}
	// Baseline preparation precedes creation of the first model step.
	return r, true, nil
}
func existingRequest(r CollaborationRun) CollaborationCreateInput {
	if r.Request == nil {
		return CollaborationCreateInput{}
	}
	return *r.Request
}
func (s *Store) CollaborationConfig(runID string, version int64) (CollaborationConfig, error) {
	var raw string
	var c CollaborationConfig
	err := s.db.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", runID, version).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c)
	}
	return c, err
}

func (s *Store) CollaborationMessages(u User, runID string, after int64) ([]CollaborationMessage, error) {
	if _, err := s.Collaboration(u, runID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT run_id,seq,id,step_id,sender_role,recipient_role,type,config_version,plan_version,reply_to,body_json,created_at FROM collaboration_messages WHERE run_id=? AND seq>? ORDER BY seq LIMIT 200", runID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollaborationMessage
	for rows.Next() {
		var m CollaborationMessage
		var raw string
		if err := rows.Scan(&m.RunID, &m.Seq, &m.ID, &m.StepID, &m.SenderRole, &m.RecipientRole, &m.Type, &m.ConfigVersion, &m.PlanVersion, &m.ReplyTo, &raw, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Body = json.RawMessage(raw)
		out = append(out, m)
	}
	return out, rows.Err()
}

const collaborationStepCols = "id,run_id,kind,role,participant_generation,config_version,plan_version,attempt,turn_id,state,error_code,revision,snapshot_id,created_at,updated_at"

func scanCollaborationStep(row interface{ Scan(...any) error }) (CollaborationStep, error) {
	var st CollaborationStep
	err := row.Scan(&st.ID, &st.RunID, &st.Kind, &st.Role, &st.ParticipantGeneration, &st.ConfigVersion, &st.PlanVersion, &st.Attempt, &st.TurnID, &st.State, &st.ErrorCode, &st.Revision, &st.SnapshotID, &st.CreatedAt, &st.UpdatedAt)
	return st, err
}
func (s *Store) CollaborationStep(id string) (CollaborationStep, error) {
	return scanCollaborationStep(s.db.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE id=?", id))
}
