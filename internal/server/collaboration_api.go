package server

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
	"agentbox/internal/gitx"
	"agentbox/internal/store"
)

func (s *Server) handleCollaborationOptions(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	type option struct {
		ID           string               `json:"id"`
		Type         string               `json:"type"`
		Label        string               `json:"label"`
		DefaultModel string               `json:"default_model"`
		Restricted   bool                 `json:"restricted"`
		Models       []config.ModelOption `json:"models"`
	}
	out := []option{}
	global := s.cfg.GetModels()
	for _, account := range s.cfg.AccountList() {
		if !s.canUseAccount(account, u.Name) {
			continue
		}
		models := account.Models
		if len(models) == 0 {
			models = global[account.Type]
		}
		visible := []config.ModelOption{}
		for _, model := range models {
			model.Reasoning = s.cfg.ConfiguredReasoning(account, model.ID)
			visible = append(visible, model)
		}
		out = append(out, option{ID: account.ID, Type: account.Type, Label: account.Label, DefaultModel: s.cfg.NewWorkspaceModel(account), Restricted: account.RestrictsModels(), Models: visible})
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "accounts": out, "thread_mode": 1, "workflow_version": 1, "automatic_intent_version": decision.Version, "decision": s.decisionAvailability(u)})
}

func (s *Server) handleCollaborationPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	var in struct {
		Scope           string                   `json:"scope"`
		SourceSessionID string                   `json:"source_session_id"`
		Repository      string                   `json:"repository"`
		Roles           store.CollaborationRoles `json:"roles"`
		Workflow        *collaboration.Workflow  `json:"workflow,omitempty"`
	}
	if err := decodeCollaboration(w, r, &in); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Scope == "" || in.Scope != s.collaborationScope(u) {
		writeProblem(w, r, "collaboration.preflight", "chat_scope_changed")
		return
	}
	if err := s.validateCollaborationBindings(u, store.CollaborationConfig{Roles: in.Roles, Workflow: in.Workflow}); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	img, err := s.dock.InspectCLIImage(ctx, s.cfg.GetAgentImage())
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	var baseline string
	err = s.workspaces().WithSession(ctx, in.SourceSessionID, func(source store.Session) error {
		if source.User != u.Name {
			return sql.ErrNoRows
		}
		managed, e := s.store.IsCollaborationSession(source.ID)
		if e != nil {
			return e
		}
		if managed {
			return collaboration.ErrInvalid
		}
		if _, e = s.sessionAccount(source); e != nil {
			return e
		}
		return s.workspaces().WithCollaborationHelper(ctx, u.Name, func() error {
			return s.dock.WithProjectReader(ctx, img.ID, s.workspaceDir(source), func(cid string) error {
				git := gitx.New(s.dock, func(context.Context, string) (string, func(), error) { return cid, func() {}, nil })
				var e error
				baseline, e = checkCollaborationSource(func(args ...string) (string, error) {
					return git.Run(ctx, source.ID, in.Repository, args...)
				})
				return e
			})
		})
	})
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "source_session_id": in.SourceSessionID, "repository": in.Repository, "baseline": baseline, "image_id": img.ID})
}

func (s *Server) handleCollaborationAbandon(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	var in struct {
		Scope string `json:"scope"`
	}
	if err := decodeCollaboration(w, r, &in); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Scope == "" || in.Scope != s.collaborationScope(u) {
		writeProblem(w, r, "collaboration.abandon", "chat_scope_changed")
		return
	}
	var err error
	if r.PathValue("action") != "" {
		run, e := s.collaborationRun(u, r.PathValue("run"))
		if e != nil {
			writeCollaborationError(w, r, e)
			return
		}
		err = s.store.AbandonCollaborationAction(u, run.ID, r.PathValue("action"))
	} else {
		err = s.store.AbandonCollaborationRequest(u, r.PathValue("request"))
	}
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "abandoned": true})
}

func (s *Server) collaborationScope(u store.User) string {
	id, err := s.clientIdentity()
	if err != nil {
		return ""
	}
	return importFingerprint([]string{"collaboration-v1", id, u.Name, u.CreatedAt.UTC().Format(time.RFC3339Nano)})
}

func (s *Server) collaborationEnabled(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !s.cfg.GetCollaborationEnabled() {
		writeProblem(w, r, "collaboration", "session_not_found")
		return false
	}
	return true
}
func decodeCollaboration(w http.ResponseWriter, r *http.Request, dst any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		return collaboration.ErrInvalid
	}
	return collaboration.Decode(raw, dst, 2<<20)
}
func writeCollaborationError(w http.ResponseWriter, r *http.Request, err error) {
	code := "internal_error"
	switch {
	case errors.Is(err, errCollaborationSourceRepository):
		code = "collaboration_source_not_repository"
	case errors.Is(err, errCollaborationSourceCommit):
		code = "collaboration_source_no_commit"
	case errors.Is(err, errCollaborationSourceDirty):
		code = "collaboration_source_dirty"
	case errors.Is(err, sql.ErrNoRows):
		code = "session_not_found"
	case errors.Is(err, store.ErrChatConflict), errors.Is(err, collaboration.ErrConflict), errors.Is(err, collaboration.ErrState):
		code = "chat_request_conflict"
	case errors.Is(err, store.ErrChatGone):
		code = "chat_request_gone"
	case errors.Is(err, store.ErrChatInvalid), errors.Is(err, collaboration.ErrInvalid), errors.Is(err, decision.ErrInvalid):
		code = "invalid_request"
	case errors.Is(err, errAccountAccess):
		code = "account_access_denied"
	}
	writeProblem(w, r, "collaboration", classifyProblem(err, code))
}
func (s *Server) collaborationRun(u store.User, id string) (store.CollaborationRun, error) {
	r, err := s.store.Collaboration(u, id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.store.CollaborationByRequest(u, id)
	}
	return r, err
}

func (s *Server) handleCollaborationList(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	var runs []store.CollaborationRun
	var err error
	if session, thread := r.URL.Query().Get("session"), r.URL.Query().Get("thread"); session != "" && thread != "" {
		source, ok := s.store.Get(session)
		if !ok || source.User != u.Name {
			writeCollaborationError(w, r, sql.ErrNoRows)
			return
		}
		runs, err = s.store.CollaborationThreadRuns(u, session, thread)
	} else {
		runs, err = s.store.ListCollaborations(u)
	}
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "runs": runs})
}
func (s *Server) handleCollaborationGet(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	cfg, err := s.store.CollaborationConfig(run.ID, run.ConfigVersion)
	if err != nil && run.State != store.CollabDeleted {
		writeCollaborationError(w, r, err)
		return
	}
	cost, stepUsage, err := s.store.CollaborationUsage(u, run.ID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	work, workErr := s.store.CollaborationWork(u, run.ID)
	if workErr != nil && !errors.Is(workErr, sql.ErrNoRows) {
		writeCollaborationError(w, r, workErr)
		return
	}
	var step *store.CollaborationStep
	if workErr == nil {
		step = &work.Step
	}
	var route *store.CollaborationDecisionRecord
	if run.Request != nil && run.Request.Decision != nil {
		d, e := s.store.CollaborationDecision(u, run.ID)
		if e != nil {
			writeCollaborationError(w, r, e)
			return
		}
		route = &d
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "run": run, "configuration": cfg, "cost": cost, "step": step, "step_usage": stepUsage, "decision": route})
}
func (s *Server) handleCollaborationCreate(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	var in store.CollaborationCreateInput
	if err := decodeCollaboration(w, r, &in); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Scope == "" || in.Scope != s.collaborationScope(u) {
		writeProblem(w, r, "collaboration.create", "chat_scope_changed")
		return
	}
	id := r.PathValue("request")
	if prior, err := s.store.CollaborationByRequest(u, id); err == nil {
		cfg := store.CollaborationConfig{CollaborationLimits: in.CollaborationLimits, Workflow: in.Workflow, Roles: in.Roles, AutoExecute: in.AutoExecute, MaxRepairs: in.MaxRepairs, MaxSteps: in.MaxSteps, BudgetMicroUSD: in.BudgetMicroUSD, VerificationCommand: in.VerificationCommand}
		_, _, err = s.store.CreateCollaboration(u, id, in, cfg)
		if err != nil {
			writeCollaborationError(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "run": prior, "replayed": true})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeCollaborationError(w, r, err)
		return
	}
	cfg := store.CollaborationConfig{CollaborationLimits: in.CollaborationLimits, Workflow: in.Workflow, Roles: in.Roles, AutoExecute: in.AutoExecute, MaxRepairs: in.MaxRepairs, MaxSteps: in.MaxSteps, BudgetMicroUSD: in.BudgetMicroUSD, VerificationCommand: in.VerificationCommand}
	if err := s.validateCollaborationBindings(u, cfg); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if s.quotaBlock(u.Name) != "" {
		writeProblem(w, r, "collaboration.create", "quota_exhausted")
		return
	}
	source, ok := s.store.Get(in.SourceSessionID)
	if !ok || source.User != u.Name {
		writeProblem(w, r, "collaboration.create", "session_not_found")
		return
	}
	managed, err := s.store.IsCollaborationSession(source.ID)
	if err != nil || managed {
		writeProblem(w, r, "collaboration.create", "invalid_request")
		return
	}
	if _, err = s.sessionAccount(source); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Context != nil {
		room := s.chat.room(source.ID)
		if err := room.begin(); err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		defer room.end()
		room.fileMu.Lock()
		defer room.fileMu.Unlock()
		if err := s.validateCollaborationContext(source, in.Context); err != nil {
			writeCollaborationError(w, r, err)
			return
		}
	}
	var run store.CollaborationRun
	var fresh bool
	if in.Decision == nil {
		run, fresh, err = s.store.CreateCollaboration(u, id, in, cfg)
	} else {
		var snapshot decision.Snapshot
		snapshot, err = s.collaborationDecisionSnapshot(u, in)
		if err == nil {
			run, fresh, err = s.store.CreateCollaborationWithDecision(u, id, in, cfg, snapshot)
		}
	}
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if fresh {
		s.kickCollaboration(u, run.ID)
	}
	status := 200
	if fresh {
		status = 202
	}
	writeJSON(w, status, map[string]any{"version": 1, "scope": s.collaborationScope(u), "run": run, "replayed": !fresh})
}
func (s *Server) handleCollaborationEvents(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			writeCollaborationError(w, r, collaboration.ErrInvalid)
			return
		}
	}
	events, err := s.store.CollaborationEvents(u, run.ID, after)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "run_id": run.ID, "events": events})
}
func (s *Server) handleCollaborationAction(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	var in store.CollaborationActionInput
	if err = decodeCollaboration(w, r, &in); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Scope == "" || in.Scope != s.collaborationScope(u) {
		writeProblem(w, r, "collaboration.action", "chat_scope_changed")
		return
	}
	// Exact replay is served before current access/configuration checks.
	if _, e := s.store.CollaborationAction(u, run.ID, r.PathValue("action")); e != nil {
		if !errors.Is(e, sql.ErrNoRows) {
			writeCollaborationError(w, r, e)
			return
		}
		if in.ExpectedRevision != run.Revision {
			writeCollaborationError(w, r, store.ErrChatConflict)
			return
		}
		if in.Action == "configure" {
			var cfg store.CollaborationConfig
			if collaboration.Decode(in.Payload, &cfg, collaboration.MaxHandoffBytes) != nil {
				writeCollaborationError(w, r, collaboration.ErrInvalid)
				return
			}
			if err = s.validateCollaborationBindings(u, cfg); err != nil {
				writeCollaborationError(w, r, err)
				return
			}
		}
		if in.Action == "repair_handoff" || in.Action == "review" || in.Action == "configure" || in.Action == "resume" {
			if run.State == store.CollabPaused || run.State == store.CollabUncertain {
				if err = s.quiesceCollaboration(r.Context(), u, run.ID, run.Revision); err != nil {
					writeCollaborationError(w, r, err)
					return
				}
			}
		}
	}
	a, fresh, err := s.store.ApplyCollaborationAction(u, run.ID, r.PathValue("action"), in)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if fresh {
		if in.Action == "cancel" {
			s.cancelCollaborationJob(run.ID)
		}
		s.kickCollaboration(u, run.ID)
	}
	writeJSON(w, 200, map[string]any{"version": 1, "action": a, "replayed": !fresh})
}
func (s *Server) handleCollaborationActionGet(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	a, err := s.store.CollaborationAction(u, run.ID, r.PathValue("action"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "action": a})
}
