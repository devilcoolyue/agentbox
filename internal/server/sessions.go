package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
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

type createSessionRequest struct {
	Name            string  `json:"name"`
	Agent           string  `json:"agent"`
	AccountID       string  `json:"account_id"`
	GitConnectionID *string `json:"git_connection_id"`
	RequestID       string  `json:"request_id,omitempty"`
	Source          string  `json:"source,omitempty"`
	Directory       string  `json:"directory,omitempty"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if key := r.PathValue("request"); key != "" {
		if req.RequestID != "" && req.RequestID != key {
			writeCreationError(w, r, store.ErrCreationConflict)
			return
		}
		req.RequestID = key
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 64 {
		writeErr(w, http.StatusBadRequest, "name is required (max 64 chars)")
		return
	}
	if req.Agent != config.AgentClaude && req.Agent != config.AgentCodex {
		writeErr(w, http.StatusBadRequest, "agent must be claude or codex")
		return
	}
	var fingerprint string
	var requestJSON []byte
	if req.RequestID != "" {
		if !operationIDPattern.MatchString(req.RequestID) {
			writeProblem(w, r, "session.create", "invalid_request")
			return
		}
		if req.Source == "" {
			req.Source = "empty"
		}
		if req.Source != "empty" && req.Source != "upload" && req.Source != "git" {
			writeProblem(w, r, "session.create", "invalid_request")
			return
		}
		if req.Directory == "" {
			req.Directory = "project"
		}
		if !validProjectDirectory(req.Directory) {
			writeProblem(w, r, "session.create", "invalid_request")
			return
		}
		payload := req
		payload.RequestID = ""
		requestJSON, _ = json.Marshal(payload)
		sum := sha256.Sum256(requestJSON)
		fingerprint = hex.EncodeToString(sum[:])
		prior, err := s.store.WorkspaceCreation(reqUser(r), req.RequestID)
		if err == nil {
			if prior.Fingerprint != fingerprint {
				writeCreationError(w, r, store.ErrCreationConflict)
				return
			}
			sess, err := s.workspaces().CreateReserved(r.Context(), reqUser(r), req.RequestID)
			if err != nil {
				writeCreationError(w, r, err)
				return
			}
			writeJSON(w, 200, s.view(sess))
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			writeCreationError(w, r, err)
			return
		}
	}
	acct, ok := s.cfg.Account(req.AccountID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown account_id")
		return
	}
	if !acct.CanUse(reqUser(r).Name, reqUser(r).Role == store.RoleAdmin) {
		writeProblem(w, r, "session.create", "account_access_denied")
		return
	}
	if acct.Type != req.Agent {
		writeErr(w, http.StatusBadRequest, "account type does not match agent type")
		return
	}

	gitConnection := ""
	if req.GitConnectionID != nil {
		gitConnection = *req.GitConnectionID
	} else {
		var err error
		gitConnection, err = s.store.GitDefault(reqUser(r).Name, "")
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
	}
	if gitConnection != "" {
		c, err := s.store.GitConnectionFor(reqUser(r).Name, gitConnection)
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
		if !c.Enabled {
			writeErr(w, 409, "默认 Git 连接已停用，请选择其他连接或不绑定")
			return
		}
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
	if req.RequestID != "" {
		// Use the larger receipt namespace; old untracked legacy directories use
		// 12-character IDs and must not be mistaken for partial preparation.
		sess.ID = newOperationID()
		_, err := s.store.ReserveWorkspaceCreation(reqUser(r), store.WorkspaceCreation{RequestID: req.RequestID, Fingerprint: fingerprint, Request: requestJSON, Session: sess, GitConnectionID: gitConnection})
		if err != nil {
			writeCreationError(w, r, err)
			return
		}
		sess, err = s.workspaces().CreateReserved(r.Context(), reqUser(r), req.RequestID)
		if err != nil {
			writeCreationError(w, r, err)
			return
		}
		writeJSON(w, 201, s.view(sess))
		return
	}
	if err := s.workspaces().CreateWithGitConnection(sess, gitConnection); err != nil {
		writeProblem(w, r, "session.create", classifyProblem(err, "internal_error"))
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
		writeProblem(w, r, "session.start", classifyProblem(err, "workspace_start_failed"))
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

// handleSwitchAccount 把工作空间改绑到同类型的另一个账号（workspace.SwitchAccount）。
// 切换要停容器，所以占住对话房间与导入：回合或导入进行中直接拒绝，不拦腰截断。
// 文件、对话记录与续聊 id 都保留，之后的对话与终端用新账号。
func (s *Server) handleSwitchAccount(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || req.AccountID == "" {
		writeProblem(w, r, "session.account", "invalid_request")
		return
	}
	acct, ok := s.cfg.Account(req.AccountID)
	if !ok || acct.Type != sess.Agent {
		writeProblem(w, r, "session.account", "invalid_request")
		return
	}
	if !s.canUseAccount(acct, sess.User) {
		writeProblem(w, r, "session.account", "account_access_denied")
		return
	}
	if req.AccountID == sess.AccountID {
		writeJSON(w, http.StatusOK, s.view(sess))
		return
	}
	room := s.chat.room(sess.ID)
	if err := room.begin(); err != nil {
		writeProblem(w, r, "session.account", chatRequestErrorCode(err))
		return
	}
	defer room.end()
	if !s.beginCreationWork(sess.ID) {
		writeProblem(w, r, "session.account", "import_pending")
		return
	}
	defer s.endCreationWork(sess.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	updated, err := s.workspaces().SwitchAccount(ctx, sess.ID, req.AccountID, s.credentialService().Release)
	if err != nil {
		code := classifyProblem(err, "internal_error")
		if errors.Is(err, workspace.ErrAccountAgent) {
			code = "invalid_request"
		}
		writeProblem(w, r, "session.account", code)
		return
	}
	log.Printf("workspace %s account %s -> %s", sess.ID, sess.AccountID, updated.AccountID)
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
