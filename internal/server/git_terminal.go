package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

//go:embed git_terminal.py
var gitTerminalHelper []byte

type gitTerminalScopeKey struct{}

type gitTerminalRegistry struct {
	mu     sync.Mutex
	grants map[string]*gitTerminalGrant
}
type gitTerminalGrant struct {
	ID                            string    `json:"id"`
	Repo                          string    `json:"repo"`
	Remote                        string    `json:"remote"`
	Write                         bool      `json:"write"`
	Expires                       time.Time `json:"expires_at"`
	Command                       string    `json:"command"`
	actor, session, login, ticket string
	binding                       store.GitBinding
	revision                      int64
	ctx                           context.Context
	cancel                        context.CancelFunc
}

func (s *Server) handleGitTerminal(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if r.Method == http.MethodGet {
		out := []*gitTerminalGrant{}
		s.gitTerminal.mu.Lock()
		for _, g := range s.gitTerminal.grants {
			if g.actor == sess.User && g.session == sess.ID && g.ctx.Err() == nil && time.Now().Before(g.Expires) {
				out = append(out, g)
			}
		}
		s.gitTerminal.mu.Unlock()
		writeJSON(w, 200, out)
		return
	}
	if r.Method == http.MethodDelete {
		s.gitTerminal.mu.Lock()
		g := s.gitTerminal.grants[r.PathValue("grant")]
		s.gitTerminal.mu.Unlock()
		if g == nil || g.actor != sess.User || g.session != sess.ID {
			writeErr(w, 404, "终端授权不存在")
			return
		}
		g.cancel()
		writeJSON(w, 200, map[string]bool{"revoked": true})
		return
	}
	var input struct {
		Repo   string `json:"repo"`
		Remote string `json:"remote"`
		Write  bool   `json:"write"`
	}
	if !decodeGitJSON(w, r, &input) {
		return
	}
	login := bearerToken(r)
	u, valid := s.store.TokenUser(login)
	if !valid || u.Name != sess.User {
		writeErr(w, 401, "登录已失效")
		return
	}
	dir, ok := s.gitRepo(w, sess, input.Repo)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	unlock, err := s.lockGit(ctx, sess, dir)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	defer unlock()
	c, b, err := s.gitBoundConnection(ctx, sess, dir, input.Remote, input.Write)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	expires := time.Now().Add(30 * time.Minute)
	grantCtx, cancelGrant := context.WithDeadline(s.workContext(), expires)
	g := &gitTerminalGrant{ID: store.NewID(), Repo: b.Repo, Remote: b.Remote, Write: input.Write, Expires: expires, actor: sess.User, session: sess.ID, login: login, ticket: oauthRandom(), binding: b, revision: c.Revision, ctx: grantCtx, cancel: cancelGrant}
	g.Command = "/home/agent/.agentbox-git/" + g.ID + "/abox-git"
	success := false
	defer func() {
		if !success {
			cancelGrant()
		}
	}()
	s.gitTerminal.mu.Lock()
	if s.gitTerminal.grants == nil {
		s.gitTerminal.grants = map[string]*gitTerminalGrant{}
	}
	n := 0
	for _, active := range s.gitTerminal.grants {
		if active.actor == sess.User {
			n++
		}
	}
	if n >= 8 || len(s.gitTerminal.grants) >= 256 {
		s.gitTerminal.mu.Unlock()
		writeErr(w, 429, "终端授权已达上限，请撤销旧授权")
		return
	}
	s.gitTerminal.grants[g.ID] = g
	s.gitTerminal.mu.Unlock()
	defer func() {
		if !success {
			s.gitTerminal.mu.Lock()
			delete(s.gitTerminal.grants, g.ID)
			s.gitTerminal.mu.Unlock()
		}
	}()
	pb := s.cfg.GetProxyBridge()
	host, _, err := net.SplitHostPort(pb.Bind)
	if err != nil {
		writeErr(w, 503, "终端授权网桥地址无效")
		return
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		writeErr(w, 503, "终端授权网桥不可用")
		return
	}
	defer func() {
		if !success {
			ln.Close()
		}
	}()
	endpoint := "http://" + net.JoinHostPort(pb.Host, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	root, err := s.openDataDir(s.homeDir(sess))
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	path := ".agentbox-git/" + g.ID
	if err = root.MkdirAll(path, 0700); err != nil {
		writeFileOpErr(w, err)
		return
	}
	// Directory ownership is required on Linux; best-effort chown supports local
	// development on hosts without permission to assign container UID 1000.
	for _, p := range []string{".agentbox-git", path} {
		if err = root.Chown(p, 1000, 1000); err != nil && !os.IsPermission(err) {
			writeFileOpErr(w, err)
			return
		}
	}
	configBytes, _ := json.Marshal(map[string]any{"url": endpoint, "token": g.ticket, "repo": g.Repo, "remote": g.Remote, "write": g.Write, "expires_at": g.Expires})
	if _, err = root.WriteFile(path+"/grant.json", configBytes, safefs.WriteOptions{Mode: 0600, Chown: true, UID: 1000, GID: 1000, BestEffortChown: true}); err != nil {
		writeFileOpErr(w, err)
		return
	}
	if _, err = root.WriteFile(path+"/abox-git", gitTerminalHelper, safefs.WriteOptions{Mode: 0700, Chown: true, UID: 1000, GID: 1000, BestEffortChown: true}); err != nil {
		writeFileOpErr(w, err)
		return
	}
	hs := &http.Server{Handler: s.admit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveGitTerminal(g, w, r) })), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10, BaseContext: func(net.Listener) context.Context { return grantCtx }}
	release, ok := s.track(func() { cancelGrant(); hs.Close(); ln.Close() })
	if !ok {
		writeErr(w, 503, "服务正在停止")
		return
	}
	stop := context.AfterFunc(grantCtx, func() { hs.Close(); ln.Close() })
	if !s.spawn(func() {
		defer release()
		defer stop()
		defer cancelGrant()
		defer func() { s.gitTerminal.mu.Lock(); delete(s.gitTerminal.grants, g.ID); s.gitTerminal.mu.Unlock() }()
		_ = hs.Serve(ln)
	}) {
		release()
		stop()
		writeErr(w, 503, "服务正在停止")
		return
	}
	success = true
	writeJSON(w, 201, g)
}

func (s *Server) serveGitTerminal(g *gitTerminalGrant, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+g.ticket)) != 1 {
		writeErr(w, 401, "终端授权无效")
		return
	}
	if g.ctx.Err() != nil || !time.Now().Before(g.Expires) {
		writeErr(w, 410, "终端授权已过期或撤销")
		return
	}
	u, ok := s.store.TokenUser(g.login)
	if !ok || u.Name != g.actor {
		writeErr(w, 401, "原登录已失效，请重新授权")
		return
	}
	sess, ok := s.store.Get(g.session)
	if !ok || sess.User != g.actor {
		writeErr(w, 403, "工作空间已失效")
		return
	}
	c, err := s.store.GitConnectionFor(g.actor, g.binding.ConnectionID)
	if err != nil || !c.Enabled || c.Revision != g.revision || g.Write && c.ReadOnly {
		writeErr(w, 403, "Git 连接权限已变化，请重新授权")
		return
	}
	bindings, err := s.store.GitBindings(g.session)
	bound := false
	for _, b := range bindings {
		if b.Repo == g.Repo && b.Remote == g.Remote && b.ConnectionID == g.binding.ConnectionID && b.URL == g.binding.URL && b.Revision == g.binding.Revision {
			bound = true
		}
	}
	if err != nil || !bound {
		writeErr(w, 403, "仓库绑定已变化，请重新授权")
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/")
	var handler func(http.ResponseWriter, *http.Request, store.Session)
	switch action {
	case "status":
		handler = s.handleGitStatus
	case "fetch":
		handler = s.handleGitFetch
	case "pull":
		handler = s.handleGitPull
	case "push-preview":
		handler = s.handleGitPushPreview
	case "push":
		handler = s.handleGitPush
	case "cancel":
	default:
		writeErr(w, 404, "操作不在授权范围")
		return
	}
	if (action == "push" || action == "push-preview") && !g.Write {
		writeErr(w, 403, "本次终端授权不允许推送")
		return
	}
	var input struct {
		Head       string `json:"expected_head"`
		RemoteHead string `json:"expected_remote_head"`
		Ref        string `json:"ref"`
		RequestID  string `json:"request_id"`
	}
	if !decodeGitJSON(w, r, &input) {
		return
	}
	if !gitRequestIDPattern.MatchString(input.RequestID) {
		writeErr(w, 400, "操作标识无效")
		return
	}
	// Scope cancellation IDs to this grant, never expose the user's other jobs.
	requestID := g.ID + "-" + input.RequestID
	if len(requestID) > 80 {
		writeErr(w, 400, "操作标识过长")
		return
	}
	internal := r.Clone(context.WithValue(context.WithValue(r.Context(), ctxUser, u), gitTerminalScopeKey{}, g))
	internal.Header = make(http.Header)
	internal.Header.Set("X-Git-Request-ID", requestID)
	internal.SetPathValue("id", g.session)
	if action == "cancel" {
		internal.SetPathValue("operation", requestID)
		s.handleGitOperationCancel(w, internal)
		return
	}
	if action == "status" {
		internal.Method = http.MethodGet
		internal.URL = &url.URL{Path: "/git/status", RawQuery: url.Values{"repo": {g.Repo}}.Encode()}
	} else {
		payload := map[string]string{"repo": g.Repo, "remote": g.Remote}
		if action == "push" || action == "push-preview" {
			payload["expected_head"] = input.Head
			payload["expected_remote_head"] = input.RemoteHead
			payload["ref"] = input.Ref
		}
		raw, _ := json.Marshal(payload)
		internal.Body = io.NopCloser(bytes.NewReader(raw))
		internal.ContentLength = int64(len(raw))
	}
	s.gitOperation("terminal."+action, s.withSession(handler)).ServeHTTP(w, internal)
}

// Checked again under the repository lock and at transport admission, so a
// binding changed after the controller's initial check cannot widen a grant.
func (s *Server) checkGitTerminalScope(ctx context.Context, c store.GitConnection, repository string, write bool) error {
	g, _ := ctx.Value(gitTerminalScopeKey{}).(*gitTerminalGrant)
	if g == nil {
		return nil
	}
	if g.ctx.Err() != nil || !time.Now().Before(g.Expires) || c.ID != g.binding.ConnectionID || c.Revision != g.revision || repository != g.binding.URL || write && !g.Write {
		return errors.New("终端授权范围已失效")
	}
	u, ok := s.store.TokenUser(g.login)
	if !ok || u.Name != g.actor {
		return errors.New("终端授权登录已失效")
	}
	bs, err := s.store.GitBindings(g.session)
	if err != nil {
		return err
	}
	for _, b := range bs {
		if b == g.binding {
			return nil
		}
	}
	return errors.New("终端授权绑定已变化")
}
