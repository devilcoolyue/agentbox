// Package server wires the HTTP API, the two WebSocket channels (PTY and
// chat) and the static web client together.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
	"agentbox/internal/dockerx"
	"agentbox/internal/gitx"
	"agentbox/internal/store"
	"agentbox/internal/web"
	"agentbox/internal/workspace"
)

// legacyUser is the single-user era placeholder; seedUsers migrates its
// sessions and data directory to the admin account on first multi-user boot.
const legacyUser = "default"

// migrateLegacyUserDir renames data/users/default to data/users/<admin> once.
// Docker bind mounts of already-running containers keep working across the
// rename; restarted containers pick up the new path from sessionDir.
func migrateLegacyUserDir(dataDir string) {
	oldDir := filepath.Join(dataDir, "users", legacyUser)
	newDir := filepath.Join(dataDir, "users", adminUser)
	if _, err := os.Stat(oldDir); err != nil {
		return
	}
	if _, err := os.Stat(newDir); !os.IsNotExist(err) {
		return
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		log.Printf("migrate legacy user dir: %v", err)
	}
}

type Server struct {
	runtimeOnce sync.Once
	serving     atomic.Bool
	life        *runtimeState
	cfg         *config.Config
	store       *store.Store
	dock        *dockerx.Manager
	git         *gitx.Runner
	chat        *chatManager
	tunnels     *tunnelHub
	pairs       *pairStore    // outstanding abox-link pairing codes
	previews    *previewStore // 短时只读的 HTML 预览通行证
	mapAuth     mapSourceAuth // source-IP cache for tunnel port-map listeners

	tunnelMu   sync.Mutex   // guards the SOCKS listener lifecycle below
	tunnelLn   net.Listener // nil when the tunnel proxy is not running
	tunnelBind string       // bind address tunnelLn was created with

	bridgeMu   sync.Mutex   // guards the account-proxy bridge lifecycle below
	bridgeLn   net.Listener // nil when the bridge is not running
	bridgeBind string       // bind address bridgeLn was created with
	bridgeUp   atomic.Bool  // set while the bridge is actually bound

	upgrader websocket.Upgrader

	startedAt  time.Time
	bootListen string // 启动时的监听地址；与配置不一致说明改动需重启

	credentialsOnce sync.Once
	credentials     *credentials.Service
	workspaceOnce   sync.Once
	workspace       *workspace.Service

	marketMu sync.Mutex // 串行化官方市场的 git 抓取（拉仓库、装技能）

	logins *loginGuard // per-IP failed-login throttle

	mon *monState // 上一帧计数器快照，供监控页按轮询间隔算 CPU 速率

	termScan *termScanner // 终端消耗补记的扫描进度（定时器与使用记录页共用）
}

func New(cfg *config.Config) (*Server, error) { return NewContext(context.Background(), cfg) }

func NewContext(ctx context.Context, cfg *config.Config) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	// Created empty so operators can discover it; an empty template is a no-op.
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "home-template"), 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return nil, err
	}
	if err := st.InitDefaultModels(cfg.GetDefaultModels()); err != nil {
		st.Close()
		return nil, err
	}
	dock, err := dockerx.NewContext(ctx, cfg)
	if err != nil {
		st.Close()
		return nil, err
	}
	s := &Server{
		cfg:        cfg,
		store:      st,
		dock:       dock,
		tunnels:    newTunnelHub(),
		pairs:      newPairStore(),
		previews:   newPreviewStore(),
		startedAt:  time.Now(),
		bootListen: cfg.GetListen(),
		logins:     newLoginGuard(),
		mon:        newMonState(),
		termScan:   &termScanner{seen: map[string]termFileState{}},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 << 10,
			WriteBufferSize: 32 << 10,
			CheckOrigin:     sameHostOrigin,
		},
	}
	s.chat = newChatManager(s)
	s.git = gitx.New(dock, s.prepareGitSession)
	if err := s.seedUsers(); err != nil {
		dock.Close()
		st.Close()
		return nil, err
	}
	s.reconcile(ctx)
	if err := ctx.Err(); err != nil {
		dock.Close()
		st.Close()
		return nil, err
	}
	return s, nil
}

// reconcile fixes session status after a server restart: a session is running
// iff its container is actually running.
func (s *Server) reconcile(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	for _, sess := range s.store.All() {
		running := s.dock.IsRunning(ctx, sess.ContainerID)
		if ctx.Err() != nil {
			return
		}
		status := store.StatusStopped
		if running {
			status = store.StatusRunning
			// Give a container that survived the restart a fresh idle window
			// instead of letting the first sweep stop it out from under a user.
			s.workspaces().Activity().Touch(sess.ID)
		}
		if sess.Status != status {
			if _, err := s.store.Update(sess.ID, func(x *store.Session) {
				x.Status = status
				if status == store.StatusRunning {
					x.StopReason = ""
				}
			}); err != nil {
				log.Printf("reconcile %s: %v", sess.ID, err)
			}
		}
	}
}

func sameHostOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// Accept both http and https origins pointing at the same host.
	trimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return strings.EqualFold(trimmed, r.Host)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/ping", s.handlePing)
	mux.Handle("POST /api/logout", s.auth(http.HandlerFunc(s.handleLogout)))
	mux.Handle("GET /api/me", s.auth(http.HandlerFunc(s.handleMe)))
	mux.Handle("POST /api/me/password", s.auth(http.HandlerFunc(s.handleChangePassword)))
	mux.Handle("GET /api/users", s.admin(http.HandlerFunc(s.handleUserList)))
	mux.Handle("POST /api/users", s.admin(http.HandlerFunc(s.handleUserCreate)))
	mux.Handle("DELETE /api/users/{name}", s.admin(http.HandlerFunc(s.handleUserDelete)))
	mux.Handle("POST /api/users/{name}/password", s.admin(http.HandlerFunc(s.handleUserSetPassword)))
	mux.Handle("GET /api/users/{name}/quota", s.admin(http.HandlerFunc(s.handleQuotaGet)))
	mux.Handle("PUT /api/users/{name}/quota", s.admin(http.HandlerFunc(s.handleQuotaSet)))
	mux.Handle("POST /api/users/{name}/credits", s.admin(http.HandlerFunc(s.handleCreditGrant)))
	mux.Handle("GET /api/usage", s.auth(http.HandlerFunc(s.handleUsageReport)))
	mux.Handle("GET /api/usage/events", s.auth(http.HandlerFunc(s.handleUsageEvents)))
	mux.Handle("GET /api/accounts", s.auth(http.HandlerFunc(s.handleAccounts)))
	mux.Handle("POST /api/accounts", s.admin(http.HandlerFunc(s.handleAccountCreate)))
	mux.Handle("PATCH /api/accounts/{id}", s.admin(http.HandlerFunc(s.handleAccountPatch)))
	mux.Handle("DELETE /api/accounts/{id}", s.admin(http.HandlerFunc(s.handleAccountDelete)))
	mux.Handle("POST /api/accounts/{id}/oauth/start", s.admin(http.HandlerFunc(s.handleOAuthStart)))
	mux.Handle("POST /api/accounts/{id}/oauth/finish", s.admin(http.HandlerFunc(s.handleOAuthFinish)))
	mux.Handle("POST /api/accounts/{id}/apikey", s.admin(http.HandlerFunc(s.handleSetAPIKey)))
	mux.Handle("DELETE /api/accounts/{id}/apikey", s.admin(http.HandlerFunc(s.handleClearAPIKey)))
	mux.Handle("POST /api/accounts/{id}/apikey/test", s.admin(http.HandlerFunc(s.handleAPIKeyTest)))
	mux.Handle("GET /api/proxies", s.admin(http.HandlerFunc(s.handleProxyList)))
	mux.Handle("POST /api/proxies", s.admin(http.HandlerFunc(s.handleProxyCreate)))
	mux.Handle("POST /api/proxies/test", s.admin(http.HandlerFunc(s.handleProxyTest)))
	mux.Handle("POST /api/proxies/import", s.admin(http.HandlerFunc(s.handleProxyImport)))
	mux.Handle("GET /api/proxies/export", s.admin(http.HandlerFunc(s.handleProxyExport)))
	mux.Handle("PATCH /api/proxies/{id}", s.admin(http.HandlerFunc(s.handleProxyPatch)))
	mux.Handle("DELETE /api/proxies/{id}", s.admin(http.HandlerFunc(s.handleProxyDelete)))
	mux.Handle("GET /api/settings", s.admin(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("PUT /api/settings", s.admin(http.HandlerFunc(s.handlePutSettings)))
	mux.Handle("GET /api/system", s.admin(http.HandlerFunc(s.handleSystem)))
	mux.Handle("GET /api/monitor", s.admin(http.HandlerFunc(s.handleMonitor)))
	mux.Handle("GET /api/sessions", s.auth(http.HandlerFunc(s.handleListSessions)))
	mux.Handle("POST /api/sessions", s.auth(http.HandlerFunc(s.handleCreateSession)))
	mux.Handle("GET /api/sessions/{id}", s.auth(s.withSession(s.handleGetSession)))
	mux.Handle("POST /api/sessions/{id}/start", s.auth(s.withSession(s.handleStartSession)))
	mux.Handle("POST /api/sessions/{id}/stop", s.auth(s.withSession(s.handleStopSession)))
	mux.Handle("PATCH /api/sessions/{id}", s.auth(s.withSession(s.handleRenameSession)))
	mux.Handle("DELETE /api/sessions/{id}", s.auth(s.withSession(s.handleDeleteSession)))
	mux.Handle("GET /api/sessions/{id}/account/usage", s.auth(s.withSession(s.handleAccountUsage)))
	mux.Handle("POST /api/sessions/{id}/upload", s.auth(s.withSession(s.handleUpload)))
	mux.Handle("GET /api/sessions/{id}/archive", s.auth(s.withSession(s.handleArchive)))
	mux.Handle("GET /api/sessions/{id}/files", s.auth(s.withSession(s.handleFiles)))
	mux.Handle("DELETE /api/sessions/{id}/files", s.auth(s.withSession(s.handleFileDelete)))
	mux.Handle("POST /api/sessions/{id}/files/move", s.auth(s.withSession(s.handleFileMove)))
	mux.Handle("POST /api/sessions/{id}/files/mkdir", s.auth(s.withSession(s.handleFileMkdir)))
	mux.Handle("POST /api/sessions/{id}/files/rename", s.auth(s.withSession(s.handleFileRename)))
	mux.Handle("GET /api/sessions/{id}/file", s.auth(s.withSession(s.handleFileGet)))
	mux.Handle("PUT /api/sessions/{id}/file", s.auth(s.withSession(s.handleFilePut)))
	mux.Handle("GET /api/sessions/{id}/preview", s.auth(s.withSession(s.handlePreviewGrant)))
	mux.Handle("POST /api/sessions/{id}/images", s.auth(s.withSession(s.handleImageUpload)))
	mux.Handle("GET /api/marketplace", s.auth(http.HandlerFunc(s.handleMarketList)))
	mux.Handle("POST /api/sessions/{id}/skills/market", s.auth(s.withSession(s.handleSkillMarketInstall)))
	mux.Handle("GET /api/sessions/{id}/skills", s.auth(s.withSession(s.handleSkillList)))
	mux.Handle("POST /api/sessions/{id}/skills", s.auth(s.withSession(s.handleSkillInstall)))
	mux.Handle("GET /api/sessions/{id}/skills/{name}", s.auth(s.withSession(s.handleSkillGet)))
	mux.Handle("GET /api/sessions/{id}/skills/{name}/file", s.auth(s.withSession(s.handleSkillFile)))
	mux.Handle("DELETE /api/sessions/{id}/skills/{name}", s.auth(s.withSession(s.handleSkillDelete)))
	mux.Handle("POST /api/sessions/{id}/skills/{name}/copy", s.auth(s.withSession(s.handleSkillCopy)))
	mux.Handle("GET /api/sessions/{id}/git/status", s.auth(s.withSession(s.handleGitStatus)))
	mux.Handle("GET /api/sessions/{id}/git/diff", s.auth(s.withSession(s.handleGitDiff)))
	mux.Handle("GET /api/sessions/{id}/git/file", s.auth(s.withSession(s.handleGitFile)))
	mux.Handle("POST /api/sessions/{id}/git/commit", s.auth(s.withSession(s.handleGitCommit)))
	mux.Handle("POST /api/sessions/{id}/git/discard", s.auth(s.withSession(s.handleGitDiscard)))
	mux.Handle("GET /api/sessions/{id}/history", s.auth(s.withSession(s.handleHistory)))
	mux.Handle("GET /api/sessions/{id}/term", s.auth(s.withSession(s.handleTermWS)))
	mux.Handle("GET /api/sessions/{id}/chat", s.auth(s.withSession(s.handleChatWS)))
	mux.Handle("GET /api/sessions/{id}/chat/threads", s.auth(s.withSession(s.handleThreadList)))
	mux.Handle("POST /api/sessions/{id}/chat/threads", s.auth(s.withSession(s.handleThreadNew)))
	mux.Handle("POST /api/sessions/{id}/chat/threads/{tid}/activate", s.auth(s.withSession(s.handleThreadActivate)))
	mux.Handle("PATCH /api/sessions/{id}/chat/threads/{tid}", s.auth(s.withSession(s.handleThreadRename)))
	mux.Handle("DELETE /api/sessions/{id}/chat/threads/{tid}", s.auth(s.withSession(s.handleThreadDelete)))
	// 旧入口：语义已并入「新建对话线程」，保留路由兼容尚未刷新的页面
	mux.Handle("POST /api/sessions/{id}/chat/reset", s.auth(s.withSession(s.handleThreadNew)))
	mux.Handle("GET /api/tunnel", s.auth(http.HandlerFunc(s.handleTunnelWS)))
	mux.Handle("GET /api/tunnel/status", s.auth(http.HandlerFunc(s.handleTunnelStatus)))
	mux.Handle("POST /api/tunnel/pair", s.auth(http.HandlerFunc(s.handleTunnelPair)))
	// Redemption is unauthenticated by design: the pairing code is the credential.
	mux.HandleFunc("POST /api/tunnel/pair/redeem", s.handleTunnelPairRedeem)
	mux.Handle("GET /api/tunnel/clients", s.auth(http.HandlerFunc(s.handleTunnelClients)))
	mux.Handle("GET /api/tunnel/clients/{name}", s.auth(http.HandlerFunc(s.handleTunnelClientGet)))

	// 预览直链自带通行证（见 preview.go），不走 s.auth
	mux.HandleFunc("GET /preview/", s.handlePreviewServe)

	mux.Handle("/", staticHandler())

	return s.admit(mux)
}

func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.bootListen)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve consumes the listener and starts background work once. Close owns
// shutdown; the application supplies cancellation instead of package signals.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if !s.serving.CompareAndSwap(false, true) {
		_ = ln.Close()
		return errors.New("server already served")
	}
	if err := ctx.Err(); err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	release, ok := s.track(func() { _ = srv.Close(); _ = ln.Close() })
	if !ok {
		_ = ln.Close()
		return errors.New("server is closed")
	}
	defer release()
	defer srv.Close()
	s.spawn(s.imageJanitor)
	s.spawn(s.credSyncLoop)
	s.spawn(s.idleReaper)
	s.spawn(s.termUsageLoop)
	s.spawn(s.termWatchLoop)
	s.spawn(s.tokenJanitor)
	if err := s.applyTunnel(); err != nil {
		log.Printf("tunnel disabled: %v", err)
	}
	if err := s.applyProxyBridge(); err != nil {
		log.Printf("account proxy bridge disabled: %v", err)
	}
	log.Printf("agentbox listening on http://%s", ln.Addr())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	var err error
	select {
	case err = <-serveErr:
	case <-ctx.Done():
		log.Printf("shutdown requested: %v", ctx.Err())
	case <-s.workContext().Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	closeErr := s.Close(shutdownCtx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, closeErr)
}

// staticHandler 用「内容哈希版本化路径」发布前端资源：index.html 里的资源
// 引用形如 /_v/<hash>/js/main.js（{{BUILD}} 占位符启动时替换），ES Module 的
// 相对 import 自动继承该前缀。内容一变哈希即变、URL 全新，所以哈希路径可以
// 放心长缓存（immutable）；而 index.html 本身 no-cache。这是唯一能穿透
// Cloudflare 的方案——CF 会把 .js/.css 的 Cache-Control 重写成 max-age=14400
// 并做边缘缓存，单纯 no-cache 到不了浏览器。
func staticHandler() http.Handler {
	dev := os.Getenv("AGENTBOX_WEB_DIR")
	var fsys fs.FS
	if dev != "" {
		log.Printf("serving web UI from %s", dev)
		fsys = os.DirFS(dev)
	} else {
		sub, err := fs.Sub(web.FS, "static")
		if err != nil {
			panic(err)
		}
		fsys = sub
	}
	build := assetBuildID(fsys)
	log.Printf("web asset build id: %s", build)
	files := http.FileServerFS(fsys)

	loadIndex := func() []byte {
		raw, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			return []byte("index.html missing")
		}
		return bytes.ReplaceAll(raw, []byte("{{BUILD}}"), []byte(build))
	}
	index := loadIndex()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" || p == "/index.html" {
			if dev != "" {
				index = loadIndex() // 开发模式磁盘热改，每次重读
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		if rest, ok := strings.CutPrefix(p, "/_v/"); ok {
			hash, sub, _ := strings.Cut(rest, "/")
			if hash == build && dev == "" {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				// 旧版本页面的哈希、或开发模式：按现行内容服务，不缓存
				w.Header().Set("Cache-Control", "no-cache")
			}
			r2 := new(http.Request)
			*r2 = *r
			u := *r.URL
			u.Path = "/" + sub
			r2.URL = &u
			files.ServeHTTP(w, r2)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// assetBuildID 对静态资源做内容哈希（含路径），资源不变则跨重启稳定。
func assetBuildID(fsys fs.FS) string {
	h := sha256.New()
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, rerr := fs.ReadFile(fsys, path)
		if rerr != nil {
			return nil
		}
		h.Write([]byte(path))
		h.Write(raw)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// withSession resolves {id} to a session owned by the caller.
func (s *Server) withSession(fn func(http.ResponseWriter, *http.Request, store.Session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.store.Get(r.PathValue("id"))
		if !ok || sess.User != reqUser(r).Name {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		fn(w, r, sess)
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := reqUser(r)
	q, metered := s.store.GetQuota(u.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":          u.Name,
		"role":          u.Role,
		"models":        s.cfg.GetModels(),
		"terminal_tips": s.cfg.GetTerminalTips(),
		"timezone":      s.cfg.GetTimeZone(),
		// 自己的额度：metered 为 false 就是不限额，前端不必显示余额。
		"quota": viewQuota(u.Name, q, metered),
	})
}

// handlePing answers a tiny, unauthenticated request the client uses to gauge
// round-trip latency to the server. no-store keeps any proxy (Cloudflare) or the
// browser from serving it from cache, which would turn the measurement into
// fiction; the client also appends a cache-busting query.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

type acctView struct {
	Access     *config.AccountAccess `json:"access,omitempty"`
	ID         string                `json:"id"`
	Type       string                `json:"type"`
	Label      string                `json:"label"`
	Sessions   int                   `json:"sessions"`
	CredStatus string                `json:"cred_status"`          // ok | norefresh | missing
	ExpiresAt  int64                 `json:"expires_at,omitempty"` // claude access token 到期(ms)
	AuthMode   string                `json:"auth_mode,omitempty"`  // claude：oauth | apikey（中转站）
	BaseURL    string                `json:"base_url,omitempty"`   // 中转站地址（codex 读 config.toml，claude 读 env）
	WireAPI    string                `json:"wire_api,omitempty"`   // codex：responses | chat
	Env        map[string]string     `json:"env,omitempty"`
	ProxyID    string                `json:"proxy_id,omitempty"` // 绑定的出口 IP 代理
	ProxyLabel string                `json:"proxy_label,omitempty"`
}

func (s *Server) accountView(a config.Account, sessions int) acctView {
	st, exp := credentials.Status(a)
	v := acctView{
		ID: a.ID, Type: a.Type, Label: a.Label, Sessions: sessions,
		CredStatus: st, ExpiresAt: exp, Env: a.Env, ProxyID: a.ProxyID, Access: a.Access,
	}
	if p, bound := s.cfg.AccountProxy(a.ID); bound {
		v.ProxyLabel = p.Name + " · " + p.DisplayURL()
	}
	switch a.Type {
	case config.AgentCodex:
		v.BaseURL, v.WireAPI = credentials.ReadCodexProvider(a.CredentialsDir)
	case config.AgentClaude:
		// env 里有中转站令牌就是 apikey 模式（实测其优先于 OAuth 凭证）
		if base, token := claudeRelay(a); token != "" {
			v.AuthMode, v.BaseURL = "apikey", base
			v.CredStatus, v.ExpiresAt = "ok", 0
		} else {
			v.AuthMode = "oauth"
		}
	}
	return v
}

// execEnv is the full per-exec env for a session: account env, the account's
// outbound IP proxy when one is bound, plus the intranet proxy variable when
// the owning user has a live reverse tunnel.
func (s *Server) execEnv(sess store.Session) ([]string, error) {
	acct, err := s.sessionAccount(sess)
	if err != nil {
		return nil, err
	}
	env := make([]string, 0, len(acct.Env))
	for k, v := range acct.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	env = append(env, s.proxyEnvList(sess)...)
	if sess.Agent == config.AgentClaude && sess.DefaultModel != "" {
		// The workspace default takes precedence over an account model override.
		filtered := env[:0]
		for _, kv := range env {
			if !strings.HasPrefix(kv, "ANTHROPIC_MODEL=") {
				filtered = append(filtered, kv)
			}
		}
		env = append(filtered, "ANTHROPIC_MODEL="+sess.DefaultModel)
	}
	return append(env, s.tunnelEnvList(sess)...), nil
}

// accountSessionCounts counts sessions per account across all users (guards
// account deletion and feeds the 设置页 display).
func (s *Server) accountSessionCounts() map[string]int {
	counts := map[string]int{}
	for _, sess := range s.store.All() {
		counts[sess.AccountID]++
	}
	return counts
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	isAdmin := reqUser(r).Role == store.RoleAdmin
	counts := map[string]int{}
	if isAdmin {
		counts = s.accountSessionCounts()
	} else {
		for _, sess := range s.store.List(reqUser(r).Name) {
			counts[sess.AccountID]++
		}
	}
	out := []acctView{}
	for _, a := range s.cfg.AccountList() {
		if !a.CanUse(reqUser(r).Name, isAdmin) {
			continue
		}
		v := s.accountView(a, counts[a.ID])
		if !isAdmin {
			// 普通用户建会话只需要账号列表本身，env/base_url 里可能有密钥，
			// 出口 IP 也属于运维信息，一并摘掉。
			v.Access = nil
			v.Env, v.BaseURL = nil, ""
			v.ProxyID, v.ProxyLabel = "", ""
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) workspaces() *workspace.Service {
	s.workspaceOnce.Do(func() {
		s.workspace = workspace.New(s.cfg, s.store, s.dock, s.sessionAccount, s.credentialService().Sync)
	})
	return s.workspace
}
