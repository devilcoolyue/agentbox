package workspace

import (
	"agentbox/internal/config"
	"agentbox/internal/store"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

type capacityRuntime struct {
	fakeRuntime
	live map[string]bool
}

func (f *capacityRuntime) Running(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id], f.fail
}
func (f *capacityRuntime) RunningWithMount(ctx context.Context, id, _ string) bool {
	b, _ := f.Running(ctx, id)
	return b
}
func TestConcurrentAdmissionUsesActualContainers(t *testing.T) {
	s, _, _ := fixture(t)
	runtime := &capacityRuntime{live: map[string]bool{}}
	s.dock = runtime
	s.cfg.Resources = config.ResourceLimits{MaxRunning: 2, MaxRunningPerUser: 1}
	// Exercise the exact critical section used by Start without filesystem/chown.
	// The Docker result is deliberately published before the saved status.
	var wg sync.WaitGroup
	successes := make(chan store.Session, 30)
	for i := 0; i < 30; i++ {
		sess := store.Session{ID: fmt.Sprint(i), User: fmt.Sprint(i % 3), ContainerID: fmt.Sprint(i), Status: store.StatusStopped}
		if err := s.store.Put(sess); err != nil {
			t.Fatal(err)
		}
	}
	for _, sess := range s.store.All() {
		if sess.ID == "s1" {
			continue
		}
		wg.Add(1)
		go func(sess store.Session) {
			defer wg.Done()
			if err := s.starts.acquire(t.Context()); err != nil {
				return
			}
			defer s.starts.release()
			if err := s.admit(t.Context(), sess); err == nil {
				runtime.mu.Lock()
				runtime.live[sess.ContainerID] = true
				runtime.mu.Unlock()
				successes <- sess
			} else if !errors.Is(err, ErrCapacity) {
				t.Error(err)
			}
		}(sess)
	}
	wg.Wait()
	close(successes)
	users := map[string]bool{}
	n := 0
	for sess := range successes {
		n++
		if users[sess.User] {
			t.Fatal("user cap exceeded")
		}
		users[sess.User] = true
	}
	if n != 2 {
		t.Fatalf("started %d, want 2", n)
	}
	runtime.fail = errors.New("daemon down")
	if err := s.admit(t.Context(), store.Session{ID: "new"}); err == nil {
		t.Fatal("daemon failure allowed startup")
	}
	runtime.fail = nil
	runtime.mu.Lock()
	clear(runtime.live)
	runtime.mu.Unlock()
	if err := s.admit(t.Context(), store.Session{ID: "new"}); err != nil {
		t.Fatal("stopped containers still consume slots", err)
	}
}

func (f *capacityRuntime) EnsureRunning(_ context.Context, sess store.Session, _ config.Account, _, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[sess.ID] = true
	f.starts++
	return sess.ID, nil
}
func TestStartAppliesAdmissionWhileOtherStartsAreInFlight(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires Linux container root for UID 1000 seeding")
	}
	s, _, original := fixture(t)
	if err := s.store.Delete(original.ID); err != nil {
		t.Fatal(err)
	}
	runtime := &capacityRuntime{live: map[string]bool{}}
	s.dock = runtime
	s.cfg.Resources = config.ResourceLimits{MaxRunning: 1}
	for i := 0; i < 8; i++ {
		sess := store.Session{ID: fmt.Sprint(i), User: "alice", Agent: config.AgentCodex, AccountID: "acct", Status: store.StatusStopped}
		if err := s.Create(sess); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, sess := range s.store.All() {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := s.Start(t.Context(), id)
			if err != nil && !errors.Is(err, ErrCapacity) {
				t.Error(err)
			}
		}(sess.ID)
	}
	wg.Wait()
	runtime.mu.Lock()
	starts := runtime.starts
	runtime.mu.Unlock()
	if starts != 1 {
		t.Fatalf("%d concurrent starts escaped cap", starts)
	}
}
