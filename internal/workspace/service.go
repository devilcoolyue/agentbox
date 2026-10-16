package workspace

import (
	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"
)

var ErrCredentials = errors.New("account credentials could not be prepared")

var ErrSessionGone = errors.New("session no longer exists")
var ErrManagedSession = errors.New("delegation worker workspace is managed by its owner")

type Store interface {
	Get(string) (store.Session, bool)
	Put(store.Session) error
	PutWithGitDefault(store.Session, string) error
	Update(string, func(*store.Session)) (store.Session, error)
	Delete(string) error
	All() []store.Session
	DelegationOwner(string) (string, bool, error)
	WorkerSessions(string) []store.Session
	CreateWorkerSession(string, store.Session) (store.Session, error)
}
type Runtime interface {
	Running(context.Context, string) (bool, error)
	RunningWithMount(context.Context, string, string) bool
	EnsureRunning(context.Context, store.Session, config.Account, string, string, string) (string, error)
	Stop(context.Context, string) error
	Remove(context.Context, string) error
}

// Service owns workspace lifecycle, activity and per-session serialization.
// Account resolution/synchronization are injected until credentials extraction.
type Service struct {
	mcp             func(context.Context, store.Session) error
	network         func(context.Context, store.Session) error
	connection      func(context.Context, store.Session, config.Account) error
	cfg             *config.Config
	store           Store
	dock            Runtime
	account         func(store.Session) (config.Account, error)
	syncCredentials func(context.Context, config.Account, store.Session) error
	activity        *Activity
	mu              sync.Mutex
	starts          sessionLock
	locks           map[string]sessionLock
}

func New(cfg *config.Config, st Store, dock Runtime, account func(store.Session) (config.Account, error), syncCredentials func(context.Context, config.Account, store.Session) error) *Service {
	return &Service{cfg: cfg, store: st, dock: dock, account: account, syncCredentials: syncCredentials, activity: NewActivity(), starts: make(sessionLock, 1), locks: map[string]sessionLock{}}
}
func (s *Service) Activity() *Activity { return s.activity }

type sessionLock chan struct{}

func (l sessionLock) acquire(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-l
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (l sessionLock) release() { <-l }
func (s *Service) lock(id string) sessionLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.locks[id]; ok {
		return l
	}
	l := make(sessionLock, 1)
	s.locks[id] = l
	return l
}
func (s *Service) SessionDir(sess store.Session) string {
	return SessionDir(s.cfg.DataDir, sess)
}

func (s *Service) WorkspaceDir(sess store.Session) string {
	return filepath.Join(s.SessionDir(sess), "workspace")
}

func (s *Service) HomeDir(sess store.Session) string {
	return filepath.Join(s.SessionDir(sess), "home")
}

// HomeTemplateDir is the server-wide skeleton overlaid onto every session home
// on start (skills, user-scope MCP servers, rc files). Empty or absent =
// feature off; see agent.SeedHomeTemplate for the merge rules.
func (s *Service) HomeTemplateDir() string {
	return filepath.Join(s.cfg.DataDir, "home-template")
}

// UserTemplateDir is the same idea scoped to one user, layered on top of the
// server-wide template. It is what the 技能 tab writes to, so a user can push
// something to all of their own sessions without touching everyone else's.
func (s *Service) UserTemplateDir(user string) string {
	return filepath.Join(s.cfg.DataDir, "users", user, "home-template")
}

// EnsureSharedDir creates (idempotently) the per-user shared directory that is
// bind-mounted into every session container at /shared.
func (s *Service) EnsureSharedDir(user string) (string, error) {
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

func (s *Service) Start(ctx context.Context, id string) (store.Session, error) {
	lock := s.lock(id)
	if err := lock.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer lock.release()

	// Re-read inside the lock: another caller may have finished the start.
	cur, ok := s.store.Get(id)
	if !ok {
		return store.Session{}, ErrSessionGone
	}
	// A delegation worker runs its own account and home against the owner's
	// files: /workspace is the owner's directory, everything else is its own.
	workspaceDir := s.WorkspaceDir(cur)
	if ownerID, worker, err := s.store.DelegationOwner(id); err != nil {
		return store.Session{}, err
	} else if worker {
		owner, ok := s.store.Get(ownerID)
		if !ok || owner.User != cur.User {
			return store.Session{}, ErrSessionGone
		}
		workspaceDir = s.WorkspaceDir(owner)
	}
	acct, err := s.account(cur)
	if err != nil {
		return store.Session{}, err
	}
	// 每次拉起（含每轮对话、终端连接）前先与账号池对齐 OAuth 令牌链，
	// 会话里刷新出的新令牌得以写回，池子的新令牌也播发进会话。
	if acct.CredentialsDir != "" {
		if err := s.syncCredentials(ctx, acct, cur); err != nil {
			return store.Session{}, fmt.Errorf("%w: %w", ErrCredentials, err)
		}
	}
	if cur.Status == store.StatusRunning && s.dock.RunningWithMount(ctx, cur.ContainerID, dockerx.SharedMount) {
		if s.network != nil {
			if err := s.network(ctx, cur); err != nil {
				return store.Session{}, err
			}
		}
		// Each chat turn and terminal starts a new CLI process, which rereads
		// its connection; extension bindings changed since the last start
		// apply without restarting the container.
		if err := s.applyConnection(ctx, cur, acct); err != nil {
			return store.Session{}, fmt.Errorf("%w: %w", ErrCredentials, err)
		}
		s.applyMCP(ctx, cur)
		s.activity.Touch(cur.ID)
		return cur, nil
	}

	if err := ctx.Err(); err != nil {
		return store.Session{}, err
	}

	if err := s.starts.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer s.starts.release()
	if err := s.admit(ctx, cur); err != nil {
		return store.Session{}, err
	}

	// Before credentials: a stray credential file in the template must never
	// outrank the account pool. A broken template shouldn't block the session
	// from coming up either, so failures are logged and the start continues.
	if err := agent.SeedHomeTemplate(s.HomeDir(cur), dockerx.AgentUID, dockerx.AgentGID,
		s.HomeTemplateDir(), s.UserTemplateDir(cur.User)); err != nil {
		log.Printf("seed home template %s: %v", cur.ID, err)
	}
	if err := agent.SeedCredentials(cur.Agent, s.HomeDir(cur), acct.CredentialsDir, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return store.Session{}, fmt.Errorf("%w: %w", ErrCredentials, err)
	}
	if err := agent.SeedDefaultModel(cur.Agent, s.HomeDir(cur), acct.ResolveModel(cur.DefaultModel), dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return store.Session{}, err
	}
	// After seeding: the pool copy just written carries the real upstream.
	if err := s.applyConnection(ctx, cur, acct); err != nil {
		return store.Session{}, fmt.Errorf("%w: %w", ErrCredentials, err)
	}
	// Only advertise the intranet proxy to the agent when the feature is on;
	// the hint keys off $AGENTBOX_INTRANET_PROXY so it stays inert if no tunnel
	// is live, but there's no reason to seed it when tunneling is disabled.
	if s.cfg.GetTunnel().Enabled {
		if err := agent.SeedIntranetHint(cur.Agent, s.HomeDir(cur), dockerx.AgentUID, dockerx.AgentGID); err != nil {
			log.Printf("seed intranet hint %s: %v", cur.ID, err)
		}
	}
	shared, err := s.EnsureSharedDir(cur.User)
	if err != nil {
		return store.Session{}, err
	}
	cid, err := s.dock.EnsureRunning(ctx, cur, acct, workspaceDir, s.HomeDir(cur), shared)
	if err != nil {
		return store.Session{}, err
	}
	s.activity.Touch(cur.ID)
	cur, err = s.store.Update(cur.ID, func(x *store.Session) {
		x.ContainerID = cid
		x.Status = store.StatusRunning
		x.StopReason = ""
	})
	if err != nil {
		return store.Session{}, err
	}
	if s.network != nil {
		if err := s.network(ctx, cur); err != nil {
			return store.Session{}, err
		}
	}
	s.applyMCP(ctx, cur)
	return cur, nil
}

func (s *Service) Create(sess store.Session) error {
	return s.CreateWithGitConnection(sess, "")
}

func (s *Service) CreateWithGitConnection(sess store.Session, connection string) error {
	if err := s.prepareDirectories(sess); err != nil {
		return err
	}
	return s.store.PutWithGitDefault(sess, connection)
}

// CreateWorker returns the owner's hidden worker workspace for an account,
// creating its home on first use. The owner lock keeps it from racing a purge.
func (s *Service) CreateWorker(ctx context.Context, owner store.Session, worker store.Session) (store.Session, error) {
	l := s.lock(owner.ID)
	if err := l.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer l.release()
	if _, ok := s.store.Get(owner.ID); !ok {
		return store.Session{}, ErrSessionGone
	}
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return store.Session{}, err
	}
	defer root.Close()
	rel, err := filepath.Rel(s.cfg.DataDir, s.HomeDir(worker))
	if err != nil {
		return store.Session{}, err
	}
	if err := root.MkdirAll(rel, 0755); err != nil {
		return store.Session{}, err
	}
	if err := root.Chown(rel, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return store.Session{}, err
	}
	return s.store.CreateWorkerSession(owner.ID, worker)
}

func (s *Service) prepareDirectories(sess store.Session) error {
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, dir := range []string{s.WorkspaceDir(sess), s.HomeDir(sess)} {
		rel, err := filepath.Rel(s.cfg.DataDir, dir)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(rel, 0755); err != nil {
			return err
		}
		if err := root.Chown(rel, dockerx.AgentUID, dockerx.AgentGID); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) Stop(ctx context.Context, id string) (store.Session, error) {
	l := s.lock(id)
	if err := l.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer l.release()
	return s.stop(ctx, id, "")
}
func (s *Service) stop(ctx context.Context, id, reason string) (store.Session, error) {
	sess, ok := s.store.Get(id)
	if !ok {
		return store.Session{}, ErrSessionGone
	}
	if sess.ContainerID != "" {
		if err := s.dock.Stop(ctx, sess.ContainerID); err != nil {
			return store.Session{}, err
		}
	}
	return s.store.Update(id, func(x *store.Session) { x.Status = store.StatusStopped; x.StopReason = reason })
}
func (s *Service) Delete(ctx context.Context, id string, purge bool) error {
	l := s.lock(id)
	if err := l.acquire(ctx); err != nil {
		return err
	}
	defer l.release()
	sess, ok := s.store.Get(id)
	if !ok {
		return ErrSessionGone
	}
	// Hidden workers mount this workspace; they go first and always purge,
	// since nothing but their own CLI state lives in their directories.
	for _, worker := range s.store.WorkerSessions(id) {
		if err := s.deleteWorker(ctx, worker); err != nil {
			return err
		}
	}
	if sess.ContainerID != "" {
		if err := s.dock.Remove(ctx, sess.ContainerID); err != nil {
			return err
		}
	}
	if err := s.store.Delete(id); err != nil {
		return err
	}
	s.activity.Forget(id)
	if purge {
		root, err := safefs.Open(s.cfg.DataDir)
		if err != nil {
			return err
		}
		defer root.Close()
		rel, err := filepath.Rel(s.cfg.DataDir, s.SessionDir(sess))
		if err != nil {
			return err
		}
		if err := root.RemoveAll(rel); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) deleteWorker(ctx context.Context, worker store.Session) error {
	l := s.lock(worker.ID)
	if err := l.acquire(ctx); err != nil {
		return err
	}
	defer l.release()
	if worker.ContainerID != "" {
		if err := s.dock.Remove(ctx, worker.ContainerID); err != nil {
			return err
		}
	}
	if err := s.store.Delete(worker.ID); err != nil {
		return err
	}
	s.activity.Forget(worker.ID)
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, err := filepath.Rel(s.cfg.DataDir, s.SessionDir(worker))
	if err != nil {
		return err
	}
	return root.RemoveAll(rel)
}

func (s *Service) Reap(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	for _, sess := range s.store.All() {
		if ctx.Err() != nil {
			return
		}
		if sess.Status != store.StatusRunning || sess.ContainerID == "" || !s.activity.Reapable(sess.ID, d) {
			continue
		}
		l := s.lock(sess.ID)
		if err := l.acquire(ctx); err != nil {
			return
		}
		cur, ok := s.store.Get(sess.ID)
		if ok && cur.Status == store.StatusRunning && s.activity.Reapable(cur.ID, d) {
			stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := s.stop(stopCtx, cur.ID, store.StopIdle)
			cancel()
			if err != nil {
				log.Printf("idle reaper stop %s: %v", cur.ID, err)
			}
		}
		l.release()
	}
}

func SessionDir(dataDir string, sess store.Session) string {
	return filepath.Join(dataDir, "users", sess.User, "sessions", sess.ID)
}

// SetNetworkHook must be called before the service is shared with callers.
func (s *Service) SetNetworkHook(hook func(context.Context, store.Session) error) { s.network = hook }

// RefreshNetwork serializes reconciliation with start, stop and deletion.
func (s *Service) RefreshNetwork(ctx context.Context, id string) error {
	lock := s.lock(id)
	if err := lock.acquire(ctx); err != nil {
		return err
	}
	defer lock.release()
	sess, ok := s.store.Get(id)
	if !ok || sess.Status != store.StatusRunning || s.network == nil {
		return nil
	}
	running, err := s.dock.Running(ctx, sess.ContainerID)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	return s.network(ctx, sess)
}

// UseRunning serializes container operations against start/stop/delete. The
// callback must be bounded: long-lived attachments are opened here, then served
// after the lock is released. Explicit stop is still allowed to disconnect them.
func (s *Service) UseRunning(ctx context.Context, id string, fn func(store.Session) error) error {
	l := s.lock(id)
	if err := l.acquire(ctx); err != nil {
		return err
	}
	defer l.release()
	sess, ok := s.store.Get(id)
	if !ok {
		return ErrSessionGone
	}
	if _, err := s.account(sess); err != nil {
		return err
	}
	running, err := s.dock.Running(ctx, sess.ContainerID)
	if err != nil {
		return err
	}
	if !running {
		return errors.New("工作空间未运行")
	}
	return fn(sess)
}

// SetConnectionHook installs the per-start connection adjustment (the Codex
// extension gateway endpoint). Call it before the service is shared.
func (s *Service) SetConnectionHook(hook func(context.Context, store.Session, config.Account) error) {
	s.connection = hook
}

func (s *Service) applyConnection(ctx context.Context, sess store.Session, acct config.Account) error {
	if s.connection == nil {
		return nil
	}
	return s.connection(ctx, sess, acct)
}

// MCP failures keep the terminal available for repair. Chat checks again before inference.
func (s *Service) SetMCPHook(hook func(context.Context, store.Session) error) { s.mcp = hook }
func (s *Service) applyMCP(ctx context.Context, sess store.Session) {
	if s.mcp != nil {
		if err := s.mcp(ctx, sess); err != nil {
			log.Printf("MCP sync pending for session %s", sess.ID)
		}
	}
}

// WithSession protects control-data edits against concurrent purge, including
// stopped workspaces. It does not start containers or require account access.
func (s *Service) WithSession(ctx context.Context, id string, fn func(store.Session) error) error {
	l := s.lock(id)
	if err := l.acquire(ctx); err != nil {
		return err
	}
	defer l.release()
	sess, ok := s.store.Get(id)
	if !ok {
		return ErrSessionGone
	}
	return fn(sess)
}
