package workspace

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func TestMaintenanceSkipsOnlyTheUserWithARunningWorkspace(t *testing.T) {
	s, _, _ := fixture(t)
	aliceCleaned, bobCleaned := false, false
	if err := s.MaintainUser(t.Context(), "alice", func() error { aliceCleaned = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if aliceCleaned {
		t.Fatal("cleaned cache of running workspace")
	}
	if err := s.MaintainUser(t.Context(), "bob", func() error { bobCleaned = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !bobCleaned {
		t.Fatal("another user's running workspace blocked this user's cleanup")
	}
}

func TestMaintenanceSerializesStarts(t *testing.T) {
	s, runtime, sess := fixture(t)
	if _, err := s.Stop(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.MaintainUser(t.Context(), sess.User, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := s.Start(ctx, sess.ID)
	if err == nil {
		t.Fatal("started during cache maintenance")
	}
	runtime.mu.Lock()
	starts := runtime.starts
	runtime.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if starts != 0 {
		t.Fatal("container started during cleanup")
	}
}

func TestFirstStartIsHeldToTheFullReserve(t *testing.T) {
	s, _, _ := fixture(t)
	s.cfg.Resources = config.ResourceLimits{MinFreeBytes: 1 << 62}
	if err := s.admit(t.Context(), store.Session{ID: "new", User: "alice"}); !errors.Is(err, ErrDiskSpace) {
		t.Fatalf("first start passed the reserve: %v", err)
	}
}
