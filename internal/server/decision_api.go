package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"agentbox/internal/collaboration"
	"agentbox/internal/decision"
	"agentbox/internal/store"
)

func (s *Server) decisionAvailability(u store.User) map[string]any {
	d := s.cfg.GetDecision()
	return map[string]any{"default_provider": d.DefaultProvider, "jev_available": d.Plugins.Jev.Enabled && d.Plugins.Jev.APIKey != "" && d.Plugins.Jev.CanUse(u.Name, u.Role == store.RoleAdmin)}
}

func (s *Server) handleDecisionReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	var in struct {
		Scope            string `json:"scope"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if err := decodeCollaboration(w, r, &in); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Scope != s.collaborationScope(u) {
		writeProblem(w, r, "collaboration.decision.reconcile", "chat_scope_changed")
		return
	}
	if err := s.store.ReconcileCollaborationDecisionAttempt(u, r.PathValue("run"), in.ExpectedRevision); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": in.Scope, "reconciled": true})
}

func (s *Server) handleCollaborationDecisionDefault(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	if sess.User != u.Name {
		writeCollaborationError(w, r, sql.ErrNoRows)
		return
	}
	if r.Method == http.MethodPut {
		var in struct {
			Scope    string `json:"scope"`
			Provider string `json:"provider"`
		}
		if err := decodeCollaboration(w, r, &in); err != nil {
			writeCollaborationError(w, r, err)
			return
		}
		if in.Scope != s.collaborationScope(u) {
			writeProblem(w, r, "collaboration.decision-default", "chat_scope_changed")
			return
		}
		if err := s.store.SetCollaborationDecisionDefault(u, sess.ID, in.Provider); err != nil {
			writeCollaborationError(w, r, err)
			return
		}
	}
	p, err := s.store.GetCollaborationDecisionDefault(u, sess.ID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "provider": p})
}

func (s *Server) collaborationDecisionSnapshot(u store.User, in store.CollaborationCreateInput) (decision.Snapshot, error) {
	var out decision.Snapshot
	if in.Decision == nil || in.Decision.Validate() != nil || (in.Workflow != nil && in.Workflow.Validate() != nil) {
		return out, decision.ErrInvalid
	}
	if in.Workflow.IsDirect() && in.Decision.Roles[collaboration.Coordinator].Mode != "fixed" {
		return out, decision.ErrInvalid
	}
	current, ok := s.store.GetUser(u.Name)
	if !ok || !current.CreatedAt.Equal(u.CreatedAt) {
		return out, errAccountAccess
	}
	// Read the policy, accounts and model capabilities together. Only the
	// nonsecret resolved candidates are serialized into the accepting transaction.
	cfg := s.cfg.CloneForDecision()
	d := cfg.GetDecision()
	provider := in.Decision.Provider
	if provider == "inherit" {
		var err error
		provider, err = s.store.GetCollaborationDecisionDefault(u, in.SourceSessionID)
		if err != nil {
			return out, err
		}
		if provider == "inherit" {
			provider = d.DefaultProvider
		}
	}
	out = decision.Snapshot{Version: decision.Version, ConfigRevision: d.Revision, Provider: provider, Roles: make(map[collaboration.Role]decision.RoleSnapshot)}
	for _, role := range []collaboration.Role{collaboration.Coordinator, collaboration.Executor} {
		binding, _ := in.Roles.Binding(role)
		if in.Workflow.IsDirect() && role == collaboration.Coordinator {
			// Retain the unused binding as part of the original intent. It does
			// not authorize a participant or enter model selection.
			out.Roles[role] = decision.RoleSnapshot{Mode: "fixed", Binding: binding, Fallback: binding}
			continue
		}
		resolved, err := decisionRoleSnapshot(cfg, current, in.Decision.Roles[role].Mode, binding)
		if err != nil {
			return out, err
		}
		out.Roles[role] = resolved
	}
	out.TemplateVersion = decision.TemplateVersion
	raw, err := json.Marshal(out.Roles)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(raw)
	out.CandidateVersion = hex.EncodeToString(sum[:])
	return out, nil
}
