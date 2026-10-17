package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

type fakeRuntime struct {
	mu                     sync.Mutex
	running                bool
	starts, stops, removes int
	fail                   error
	entered                chan struct{}
	proceed                chan struct{}
}

func (f *fakeRuntime) RunningWithMount(context.Context, string, string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}
func (f *fakeRuntime) EnsureRunning(ctx context.Context, _ store.Session, _ config.Account, _, _, _ string) (string, error) {
	if f.entered != nil {
		close(f.entered)
		select {
		case <-f.proceed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.running = true
	return "cid", nil
}
func (f *fakeRuntime) Stop(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.fail != nil {
		return f.fail
	}
	f.running = false
	return nil
}
func (f *fakeRuntime) Remove(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	if f.fail != nil {
		return f.fail
	}
	f.running = false
	return nil
}
func fixture(t *testing.T) (*Service, *fakeRuntime, store.Session) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sess := store.Session{ID: "s1", User: "alice", Agent: config.AgentCodex, AccountID: "acct", ContainerID: "cid", Status: store.StatusRunning}
	if err := st.Put(sess); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{running: true}
	service := New(&config.Config{DataDir: root}, st, runtime, func(store.Session) (config.Account, error) {
		return config.Account{ID: "acct", Type: config.AgentCodex}, nil
	}, func(context.Context, config.Account, store.Session) error { return nil })
	return service, runtime, sess
}
func TestStopDeleteFailuresKeepPersistentSession(t *testing.T) {
	s, r, sess := fixture(t)
	r.fail = errors.New("Docker unavailable")
	if _, err := s.Stop(t.Context(), sess.ID); err == nil {
		t.Fatal("stop swallowed error")
	}
	if err := s.Delete(t.Context(), sess.ID, true); err == nil {
		t.Fatal("delete swallowed error")
	}
	cur, ok := s.store.Get(sess.ID)
	if !ok || cur.Status != store.StatusRunning {
		t.Fatalf("lost running session on runtime failure: %+v", cur)
	}
	r.fail = nil
	if err := s.Delete(t.Context(), sess.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(t.Context(), sess.ID); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("deleted session resurrected: %v", err)
	}
	if r.starts != 0 {
		t.Fatal("started deleted session")
	}
}
func TestStartSerializesAndLockWaitCanCancel(t *testing.T) {
	s, r, sess := fixture(t)
	l := s.lock(sess.ID)
	if err := l.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Start(ctx, sess.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock wait ignored cancel: %v", err)
	}
	l.release()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Start(t.Context(), sess.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if r.starts != 0 {
		t.Fatal("running session recreated")
	}
}
func TestReapRespectsActivityAndStoppedState(t *testing.T) {
	s, r, sess := fixture(t)
	s.activity.Hold(sess.ID)
	s.activity.mu.Lock()
	s.activity.seen[sess.ID].lastSeen = time.Now().Add(-time.Hour)
	s.activity.mu.Unlock()
	s.Reap(t.Context(), time.Minute)
	if r.stops != 0 {
		t.Fatal("active session reaped")
	}
	s.activity.Release(sess.ID)
	s.activity.mu.Lock()
	s.activity.seen[sess.ID].lastSeen = time.Now().Add(-time.Hour)
	s.activity.mu.Unlock()
	s.Reap(t.Context(), time.Minute)
	cur, _ := s.store.Get(sess.ID)
	if r.stops != 1 || cur.StopReason != store.StopIdle {
		t.Fatalf("not reaped: %+v", cur)
	}
	s.Reap(t.Context(), time.Minute)
	if r.stops != 1 {
		t.Fatal("stopped twice")
	}
}
func TestConcurrentStartThenDeleteCannotLeaveOrphanContainer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires chown to container UID; exercised in Linux container suite")
	}
	s, r, sess := fixture(t)
	sess.Status = store.StatusStopped
	sess.ContainerID = ""
	r.running = false
	if err := s.Create(sess); err != nil {
		t.Fatal(err)
	}
	r.entered = make(chan struct{})
	r.proceed = make(chan struct{})
	started := make(chan error, 1)
	deleted := make(chan error, 1)
	go func() { _, err := s.Start(t.Context(), sess.ID); started <- err }()
	select {
	case <-r.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("start never reached Docker")
	}
	go func() { deleted <- s.Delete(t.Context(), sess.ID, true) }()
	close(r.proceed)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if _, ok := s.store.Get(sess.ID); ok {
		t.Fatal("session retained")
	}
	if r.running || r.starts != 1 || r.removes != 1 {
		t.Fatalf("orphan runtime: %+v", r)
	}
	if _, err := os.Stat(s.SessionDir(sess)); !os.IsNotExist(err) {
		t.Fatalf("purge: %v", err)
	}
}

func (f *fakeRuntime) Running(ctx context.Context, id string) (bool, error) {
	return f.RunningWithMount(ctx, id, ""), f.fail
}

func TestMCPHookRunsForRunningWorkspaceAndKeepsRepairAccess(t *testing.T) {
	s, _, sess := fixture(t)
	calls := 0
	s.SetMCPHook(func(context.Context, store.Session) error { calls++; return errors.New("synthetic MCP conflict") })
	for range 2 {
		if _, err := s.Start(t.Context(), sess.ID); err != nil {
			t.Fatal("MCP conflict blocked terminal repair", err)
		}
	}
	if calls != 2 {
		t.Fatalf("hook skipped running workspace: %d", calls)
	}
	if err := s.Delete(t.Context(), sess.ID, false); err != nil {
		t.Fatal(err)
	}
	invoked := false
	if err := s.WithSession(t.Context(), sess.ID, func(store.Session) error { invoked = true; return nil }); !errors.Is(err, ErrSessionGone) || invoked {
		t.Fatal("control mutation resurrected deleted workspace")
	}
}

type mountRuntime struct {
	fakeRuntime
	workspace, home string
}

func (f *mountRuntime) EnsureRunning(ctx context.Context, sess store.Session, acct config.Account, workspace, home, shared string) (string, error) {
	f.workspace, f.home = workspace, home
	return f.fakeRuntime.EnsureRunning(ctx, sess, acct, workspace, home, shared)
}

// A delegation worker mounts its owner's files with its own home, cannot be
// rebound by itself, and leaves together with its owner.
func TestWorkerWorkspaceMountsOwnerFilesAndLeavesWithOwner(t *testing.T) {
	if uid := os.Geteuid(); uid != 0 && uid != 1000 {
		t.Skip("requires chown to container UID; exercised in Linux container suite")
	}
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runtime := &mountRuntime{}
	s := New(&config.Config{DataDir: root}, st, runtime, func(sess store.Session) (config.Account, error) {
		return config.Account{ID: sess.AccountID, Type: sess.Agent}, nil
	}, func(context.Context, config.Account, store.Session) error { return nil })
	owner := store.Session{ID: "owner", User: "alice", Agent: config.AgentClaude, AccountID: "main", Status: store.StatusStopped}
	if err = s.Create(owner); err != nil {
		t.Fatal(err)
	}
	worker, err := s.CreateWorker(t.Context(), owner, store.Session{ID: "worker", User: "alice", Agent: config.AgentCodex, AccountID: "cx", Status: store.StatusStopped})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(t.Context(), worker.ID); err != nil {
		t.Fatal(err)
	}
	if runtime.workspace != s.WorkspaceDir(owner) || runtime.home != s.HomeDir(worker) {
		t.Fatalf("worker mounts workspace=%s home=%s", runtime.workspace, runtime.home)
	}
	if _, err = s.SwitchAccount(t.Context(), worker.ID, "other", nil); !errors.Is(err, ErrManagedSession) {
		t.Fatal("worker account switched directly", err)
	}
	if err = s.Delete(t.Context(), owner.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get(worker.ID); ok {
		t.Fatal("worker survived its owner")
	}
	if _, err = os.Stat(s.SessionDir(worker)); !os.IsNotExist(err) {
		t.Fatal("worker home kept", err)
	}
	if _, err = os.Stat(s.WorkspaceDir(owner)); err != nil {
		t.Fatal("unpurged owner files removed", err)
	}
	if runtime.removes != 1 {
		t.Fatal("worker container not removed", runtime.removes)
	}
}

// The connection hook (Codex model gateway endpoint) runs on every start:
// a running container's next CLI rereads its config, and a fresh start applies
// it after seeding so the pool copy cannot overwrite it. Its failure blocks the
// start instead of letting the CLI bypass the gateway.
func TestConnectionHookRunsOnEveryStartAfterSeeding(t *testing.T) {
	s, runtime, sess := fixture(t)
	var calls int
	var fail error
	s.SetConnectionHook(func(_ context.Context, got store.Session, acct config.Account) error {
		calls++
		if got.ID != sess.ID || acct.ID != "acct" {
			t.Errorf("hook got %s/%s", got.ID, acct.ID)
		}
		return fail
	})
	if _, err := s.Start(t.Context(), sess.ID); err != nil || calls != 1 {
		t.Fatalf("running start: calls=%d err=%v", calls, err)
	}
	if err := os.MkdirAll(s.HomeDir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.running = false
	runtime.mu.Unlock()
	if _, err := s.Start(t.Context(), sess.ID); err != nil || calls != 2 || runtime.starts != 1 {
		t.Fatalf("fresh start: calls=%d starts=%d err=%v", calls, runtime.starts, err)
	}
	fail = errors.New("synthetic endpoint failure")
	if _, err := s.Start(t.Context(), sess.ID); !errors.Is(err, ErrCredentials) || !errors.Is(err, fail) {
		t.Fatalf("hook failure did not block the start: %v", err)
	}
}

// The lifecycle hook reports transitions only: a start that finds the
// container running, or a stop of a stopped workspace, is not an event.
func TestLifecycleHookReportsTransitions(t *testing.T) {
	s, runtime, sess := fixture(t)
	var got []string
	s.SetLifecycleHook(func(action, reason string, x store.Session) {
		got = append(got, action+":"+reason+":"+x.ID)
	})
	if _, err := s.Start(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.HomeDir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(t.Context(), sess.ID); err != nil || runtime.starts != 1 {
		t.Fatalf("cold start: %v", err)
	}
	created := store.Session{ID: "s2", User: "alice", Agent: config.AgentCodex, AccountID: "acct"}
	if err := s.Create(created); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), created.ID, false); err != nil {
		t.Fatal(err)
	}
	want := []string{"stopped::s1", "started::s1", "created::s2", "deleted::s2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", got, want)
	}
}
