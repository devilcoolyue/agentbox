package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"agentbox/internal/mcpconfig"
	"agentbox/internal/store"
)

func (s *Server) mcpService() *mcpconfig.Service {
	s.mcpOnce.Do(func() { s.mcp = mcpconfig.New(s.cfg.DataDir) })
	return s.mcp
}
func mcpError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, mcpconfig.ErrRevision) || errors.Is(err, mcpconfig.ErrConflict) {
		code = http.StatusConflict
	}
	writeErr(w, code, err.Error())
}
func mcpBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeErr(w, 400, "MCP 请求格式无效")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "MCP 请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func (s *Server) handleMCPUser(w http.ResponseWriter, r *http.Request) {
	s.handleMCPConfig(w, r, reqUser(r).Name, "")
}
func (s *Server) handleMCPSession(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if sess.Agent != "claude" {
		writeErr(w, 400, "MCP 管理首版仅支持 Claude")
		return
	}
	if r.Method == http.MethodGet {
		s.handleMCPConfig(w, r, sess.User, sess.ID)
		return
	}
	if err := s.workspaces().WithSession(r.Context(), sess.ID, func(cur store.Session) error { s.handleMCPConfig(w, r, cur.User, cur.ID); return nil }); err != nil {
		writeErr(w, 404, "工作空间已删除")
	}
}
func (s *Server) handleMCPConfig(w http.ResponseWriter, r *http.Request, user, id string) {
	svc := s.mcpService()
	if r.Method == http.MethodGet {
		v, err := svc.View(user, id)
		if err != nil {
			mcpError(w, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	var req struct {
		Revision int64           `json:"revision"`
		Entry    mcpconfig.Entry `json:"entry"`
	}
	if !mcpBody(w, r, &req) {
		return
	}
	var entry *mcpconfig.Entry
	if r.Method == http.MethodPut {
		entry = &req.Entry
	}
	if err := svc.Put(user, id, r.PathValue("name"), req.Revision, entry); err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleMCPAdopt(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if sess.Agent != "claude" {
		writeErr(w, 400, "仅支持 Claude")
		return
	}
	var req struct {
		Revision       int64  `json:"revision"`
		NativeRevision string `json:"native_revision"`
		Release        bool   `json:"release"`
	}
	if !mcpBody(w, r, &req) {
		return
	}
	if err := s.workspaces().WithSession(r.Context(), sess.ID, func(cur store.Session) error {
		return s.mcpService().Adopt(cur.User, cur.ID, r.PathValue("name"), req.Revision, req.NativeRevision, req.Release)
	}); err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// runMCP never stores or returns raw child output/errors. Keep stdin open after
// the request: EOF is the helper's cancellation channel. The helper also has an
// independent deadline and kills process groups, even if Docker disconnects.
func (s *Server) runMCP(ctx context.Context, sess store.Session, payload any) (mcpconfig.CheckResult, error) {
	var result mcpconfig.CheckResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	env, err := s.execEnv(sess)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, ok := s.track(cancel)
	if !ok {
		return result, errors.New("服务正在停止")
	}
	defer release()
	execCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 32*time.Second)
	defer stop()
	stream, err := s.dock.ExecStream(execCtx, sess.ContainerID, []string{"python3", "-I", "-c", mcpconfig.Helper}, env)
	if err != nil {
		return result, errors.New("无法启动 MCP 容器检测程序")
	}
	defer stream.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
			_ = stream.CloseWrite()
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				stop()
			}
		}
	}()
	raw, err := json.Marshal(payload)
	if err != nil {
		return result, err
	}
	if _, err = stream.Write(append(raw, '\n')); err != nil {
		return result, errors.New("无法发送 MCP 配置")
	}
	out := &mcpOutput{}
	if err = stream.Demux(out, io.Discard); err != nil {
		return result, errors.New("MCP 操作中断")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if out.overflow || json.Unmarshal(out.Bytes(), &result) != nil {
		return result, errors.New("MCP 检测返回无效结果")
	}
	result.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	return result, nil
}

type mcpOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *mcpOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) - b.Len()
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func (s *Server) syncMCP(ctx context.Context, sess store.Session) error {
	if sess.Agent != "claude" {
		return nil
	}
	return s.mcpService().Sync(ctx, sess.User, sess.ID, func(ctx context.Context, name string, expected, desired *mcpconfig.Definition) error {
		result, err := s.runMCP(ctx, sess, map[string]any{"action": "apply", "name": name, "expected": expected, "desired": desired})
		if err != nil {
			return err
		}
		if result.Status != "applied" {
			return errors.New("MCP 应用失败")
		}
		return nil
	})
}
func (s *Server) handleMCPCheck(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if sess.Agent != "claude" {
		writeErr(w, 400, "仅支持 Claude")
		return
	}
	if _, err := s.sessionAccount(sess); err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	if why := s.quotaBlock(sess.User); why != "" {
		writeErr(w, 403, why)
		return
	}
	s.mcpCheckMu.Lock()
	if s.mcpChecks == nil {
		s.mcpChecks = map[string]bool{}
	}
	busy := s.mcpChecks[sess.ID]
	if !busy {
		s.mcpChecks[sess.ID] = true
	}
	s.mcpCheckMu.Unlock()
	if busy {
		writeErr(w, 409, "该空间已有 MCP 检测正在运行")
		return
	}
	defer func() { s.mcpCheckMu.Lock(); delete(s.mcpChecks, sess.ID); s.mcpCheckMu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	release, ok := s.track(cancel)
	if !ok {
		writeErr(w, 503, "服务正在停止")
		return
	}
	defer release()
	definition, err := s.mcpService().Definition(sess.User, sess.ID, r.PathValue("name"))
	if err != nil {
		mcpError(w, err)
		return
	}
	sess, err = s.startSession(ctx, sess)
	if err != nil {
		writeErr(w, 409, "无法启动工作空间")
		return
	}
	s.workspaces().Activity().Hold(sess.ID)
	defer s.workspaces().Activity().Release(sess.ID)
	var result mcpconfig.CheckResult
	err = s.workspaces().UseRunning(ctx, sess.ID, func(cur store.Session) error {
		if why := s.quotaBlock(cur.User); why != "" {
			return errors.New(why)
		}
		var e error
		result, e = s.runMCP(ctx, cur, map[string]any{"action": "check", "config": definition})
		return e
	})
	if err != nil {
		writeErr(w, 409, "MCP 检测中断或工作空间不可用，请重试")
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleMCPImport(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		Revision int64                           `json:"revision"`
		Servers  map[string]mcpconfig.Definition `json:"mcpServers"`
	}
	if !mcpBody(w, r, &req) {
		return
	}
	if err := s.mcpService().Import(user, id, req.Revision, req.Servers); err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleMCPUserImport(w http.ResponseWriter, r *http.Request) {
	s.handleMCPImport(w, r, reqUser(r).Name, "")
}
func (s *Server) handleMCPSessionImport(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if sess.Agent != "claude" {
		writeErr(w, 400, "仅支持 Claude")
		return
	}
	if err := s.workspaces().WithSession(r.Context(), sess.ID, func(cur store.Session) error { s.handleMCPImport(w, r, cur.User, cur.ID); return nil }); err != nil {
		writeErr(w, 404, "工作空间已删除")
	}
}
func (s *Server) handleMCPCopy(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if sess.Agent != "claude" {
		writeErr(w, 400, "仅支持 Claude")
		return
	}
	var req struct {
		Revision int64 `json:"revision"`
	}
	if !mcpBody(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	def, err := s.mcpService().Definition(sess.User, sess.ID, name)
	if err != nil {
		mcpError(w, err)
		return
	}
	// Import refuses overwriting an existing user definition, and never sends a
	// secret through the browser while copying between scopes.
	if err = s.mcpService().Import(reqUser(r).Name, "", req.Revision, map[string]mcpconfig.Definition{name: def}); err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Defer incidental start/terminal/Git synchronization while a web turn is in
// flight. runTurn synchronizes explicitly before starting its own CLI.
func (s *Server) syncMCPOnStart(ctx context.Context, sess store.Session) error {
	if sess.Agent != "claude" {
		return nil
	}
	if s.chat != nil {
		s.chat.mu.Lock()
		room := s.chat.rooms[sess.ID]
		s.chat.mu.Unlock()
		if room != nil {
			room.mu.Lock()
			running := room.running
			room.mu.Unlock()
			if running {
				return nil
			}
		}
	}
	return s.syncMCP(ctx, sess)
}
