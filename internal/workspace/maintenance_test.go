package workspace

import (
	"context"
	"testing"
	"time"
)

func TestMaintenanceSkipsRunningAndSerializesStarts(t *testing.T) {
	s, runtime, sess := fixture(t)
	called := false
	if err := s.MaintainStopped(t.Context(), func() error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("cleaned cache of running workspace")
	}
	if _, err := s.Stop(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- s.MaintainStopped(t.Context(), func() error { close(entered); <-release; return nil }) }()
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
