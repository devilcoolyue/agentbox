package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

func (s *Server) sessionDir(sess store.Session) string {
	return filepath.Join(s.cfg.DataDir, "users", sess.User, "sessions", sess.ID)
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
	dir := filepath.Join(s.cfg.DataDir, "users", user, "shared")
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	rel := filepath.Join("users", user, "shared")
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return "", err
	}
	if err := root.Chown(rel, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return "", err
	}

	return dir, nil
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
	for _, dir := range []string{s.workspaceDir(sess), s.homeDir(sess)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.Chown(dir, dockerx.AgentUID, dockerx.AgentGID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.store.Put(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.view(sess))
}

// startSession is the idempotent bring-up used by the REST endpoint and by
// the chat/terminal channels (which auto-start stopped sessions).
func (s *Server) startSession(ctx context.Context, sess store.Session) (store.Session, error) {
	lock := s.startLock(sess.ID)
	lock.Lock()
	defer lock.Unlock()

	// Re-read inside the lock: another caller may have finished the start.
	cur, ok := s.store.Get(sess.ID)
	if !ok {
		return store.Session{}, errSessionGone
	}
	acct, ok := s.cfg.Account(cur.AccountID)
	if !ok {
		return store.Session{}, errAccountGone
	}
	// 每次拉起（含每轮对话、终端连接）前先与账号池对齐 OAuth 令牌链，
	// 会话里刷新出的新令牌得以写回，池子的新令牌也播发进会话。
	if acct.CredentialsDir != "" {
		s.syncRotatingCred(acct, cur)
	}
	if cur.Status == store.StatusRunning && s.dock.RunningWithMount(ctx, cur.ContainerID, dockerx.SharedMount) {
		s.idle.touch(cur.ID)
		return cur, nil
	}

	// Before credentials: a stray credential file in the template must never
	// outrank the account pool. A broken template shouldn't block the session
	// from coming up either, so failures are logged and the start continues.
	if err := agent.SeedHomeTemplate(s.homeDir(cur), dockerx.AgentUID, dockerx.AgentGID,
		s.homeTemplateDir(), s.userTemplateDir(cur.User)); err != nil {
		log.Printf("seed home template %s: %v", cur.ID, err)
	}
	if err := agent.SeedCredentials(cur.Agent, s.homeDir(cur), acct.CredentialsDir, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return store.Session{}, err
	}
	if err := agent.SeedDefaultModel(cur.Agent, s.homeDir(cur), cur.DefaultModel, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return store.Session{}, err
	}
	// Only advertise the intranet proxy to the agent when the feature is on;
	// the hint keys off $AGENTBOX_INTRANET_PROXY so it stays inert if no tunnel
	// is live, but there's no reason to seed it when tunneling is disabled.
	if s.cfg.GetTunnel().Enabled {
		if err := agent.SeedIntranetHint(cur.Agent, s.homeDir(cur), dockerx.AgentUID, dockerx.AgentGID); err != nil {
			log.Printf("seed intranet hint %s: %v", cur.ID, err)
		}
	}
	shared, err := s.ensureSharedDir(cur.User)
	if err != nil {
		return store.Session{}, err
	}
	cid, err := s.dock.EnsureRunning(ctx, cur, acct, s.workspaceDir(cur), s.homeDir(cur), shared)
	if err != nil {
		return store.Session{}, err
	}
	s.idle.touch(cur.ID)
	return s.store.Update(cur.ID, func(x *store.Session) {
		x.ContainerID = cid
		x.Status = store.StatusRunning
		x.StopReason = "" // 又跑起来了，清掉上一次的休眠标记
	})
}

var (
	errSessionGone = jsonError("session no longer exists")
	errAccountGone = jsonError("account referenced by session is gone from config")
)

type jsonError string

func (e jsonError) Error() string { return string(e) }

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	updated, err := s.startSession(ctx, sess)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
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
	if sess.ContainerID != "" {
		if err := s.dock.Stop(ctx, sess.ContainerID); err != nil {
			log.Printf("stop %s: %v", sess.ID, err)
		}
	}
	updated, err := s.store.Update(sess.ID, func(x *store.Session) {
		x.Status = store.StatusStopped
		x.StopReason = "" // 用户主动停的，不是休眠
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(updated))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if sess.ContainerID != "" {
		if err := s.dock.Remove(ctx, sess.ContainerID); err != nil {
			log.Printf("remove container of %s: %v", sess.ID, err)
		}
	}
	if err := s.store.Delete(sess.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.idle.forget(sess.ID)
	if r.URL.Query().Get("purge") == "1" {
		if err := os.RemoveAll(s.sessionDir(sess)); err != nil {
			writeErr(w, http.StatusInternalServerError, "session deleted but purge failed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
