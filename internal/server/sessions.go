package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/internal/workspace"
)

func (s *Server) sessionDir(sess store.Session) string {
	return workspace.SessionDir(s.cfg.DataDir, sess)
}

func (s *Server) workspaceDir(sess store.Session) string {
	return filepath.Join(s.sessionDir(sess), "workspace")
}

func (s *Server) homeDir(sess store.Session) string {
	return filepath.Join(s.sessionDir(sess), "home")
}

func (s *Server) chatLogPath(sess store.Session) string {
	return filepath.Join(s.sessionDir(sess), "chat.jsonl")
}

// homeTemplateDir is the server-wide skeleton overlaid onto every session home
// on start (skills, user-scope MCP servers, rc files). Empty or absent =
// feature off; see agent.SeedHomeTemplate for the merge rules.
func (s *Server) homeTemplateDir() string {
	return filepath.Join(s.cfg.DataDir, "home-template")
}

// userTemplateDir is the same idea scoped to one user, layered on top of the
// server-wide template. It is what the 技能 tab writes to, so a user can push
// something to all of their own sessions without touching everyone else's.
func (s *Server) userTemplateDir(user string) string {
	return filepath.Join(s.cfg.DataDir, "users", user, "home-template")
}

// ensureSharedDir creates (idempotently) the per-user shared directory that is
// bind-mounted into every session container at /shared.
func (s *Server) ensureSharedDir(user string) (string, error) {
	return s.workspaces().EnsureSharedDir(user)
}

type sessionView struct {
	store.Session
	AccountLabel string `json:"account_label"`
}

func (s *Server) view(sess store.Session) sessionView {
	label := sess.AccountID
	if a, ok := s.cfg.Account(sess.AccountID); ok {
		label = a.Label
	}
	return sessionView{Session: sess, AccountLabel: label}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	out := []sessionView{}
	for _, sess := range s.store.List(reqUser(r).Name) {
		out = append(out, s.view(sess))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	writeJSON(w, http.StatusOK, s.view(sess))
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Agent     string `json:"agent"`
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" || len(req.Name) > 64 {
		writeErr(w, http.StatusBadRequest, "name is required (max 64 chars)")
		return
	}
	if req.Agent != config.AgentClaude && req.Agent != config.AgentCodex {
		writeErr(w, http.StatusBadRequest, "agent must be claude or codex")
		return
	}
	acct, ok := s.cfg.Account(req.AccountID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown account_id")
		return
	}
	if !acct.CanUse(reqUser(r).Name, reqUser(r).Role == store.RoleAdmin) {
		writeErr(w, http.StatusForbidden, errAccountAccess.Error())
		return
	}
	if acct.Type != req.Agent {
		writeErr(w, http.StatusBadRequest, "account type does not match agent type")
		return
	}

	sess := store.Session{
		ID:           store.NewID(),
		User:         reqUser(r).Name,
		Name:         req.Name,
		Agent:        req.Agent,
		AccountID:    req.AccountID,
		DefaultModel: s.cfg.GetDefaultModel(req.Agent),
		Status:       store.StatusStopped,
		CreatedAt:    time.Now(),
	}
	if err := s.workspaces().Create(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.view(sess))
}

// startSession is the idempotent bring-up used by the REST endpoint and by
// the chat/terminal channels (which auto-start stopped sessions).
func (s *Server) startSession(ctx context.Context, sess store.Session) (store.Session, error) {
	return s.workspaces().Start(ctx, sess.ID)
}

var (
	errSessionGone = workspace.ErrSessionGone
	errAccountGone = jsonError("account referenced by session is gone from config")
)

type jsonError string

func (e jsonError) Error() string { return string(e) }

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	updated, err := s.startSession(ctx, sess)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, workspace.ErrCapacity) {
			status = http.StatusTooManyRequests
		}
		if err == errAccountAccess {
			status = http.StatusForbidden
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(updated))
}

// handleRenameSession changes a session's display name. Only the label moves —
// the id, directories and container name are all derived from the id, so a
// rename touches nothing else.
func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 64 {
		writeErr(w, http.StatusBadRequest, "工作空间名称需为 1-64 个字符")
		return
	}
	updated, err := s.store.Update(sess.ID, func(x *store.Session) { x.Name = name })
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(updated))
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	updated, err := s.workspaces().Stop(ctx, sess.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(updated))
}
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.workspaces().Delete(ctx, sess.ID, r.URL.Query().Get("purge") == "1"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
