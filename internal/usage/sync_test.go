package usage

import (
	"agentbox/internal/config"
	"agentbox/internal/store"
	"context"
	"testing"
	"time"
)

type blockedScanStore struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockedScanStore) All() []store.Session                               { close(b.entered); <-b.release; return nil }
func (*blockedScanStore) Get(string) (store.Session, bool)                     { return store.Session{}, false }
func (*blockedScanStore) InsertUsage(...store.UsageEvent) error                { return nil }
func (*blockedScanStore) InsertUsageMessages(...store.UsageEvent) (int, error) { return 0, nil }
func (*blockedScanStore) UpsertTerminalUsage(...store.UsageEvent) error        { return nil }
func TestQueriesCanRequestSyncWhileScanIsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	st := &blockedScanStore{make(chan struct{}), make(chan struct{})}
	s := New(ctx, &config.Config{}, st)
	done := make(chan struct{})
	go func() { s.Loop(); close(done) }()
	<-st.entered
	for i := 0; i < 10000; i++ {
		s.RequestScan()
	}
	if !s.SyncStatus().Scanning {
		t.Fatal("scan status unavailable")
	}
	close(st.release)
	deadline := time.Now().Add(time.Second)
	for s.SyncStatus().LastSuccessAt == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.SyncStatus().LastSuccessAt == 0 {
		t.Fatal("missing completion timestamp")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker leaked")
	}
}
