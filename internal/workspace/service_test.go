package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
