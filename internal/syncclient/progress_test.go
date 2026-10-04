package syncclient

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestProgressCoalescesWithoutBlockingProducer(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var mu sync.Mutex
	var values []Progress
	pump := startProgressPump(t.Context(), func(p Progress) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		values = append(values, p)
		mu.Unlock()
	})
	pump.offer(Progress{Sequence: 1})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no progress emitted")
	}
	produced := make(chan struct{})
	go func() {
		for i := uint64(2); i <= 100_000; i++ {
			pump.offer(Progress{Sequence: i})
		}
		close(produced)
	}()
	select {
	case <-produced:
	case <-time.After(time.Second):
		t.Fatal("progress blocked executor")
	}
	once.Do(func() { close(release) })
	pump.finish()
	mu.Lock()
	defer mu.Unlock()
	if len(values) != 2 || values[1].Sequence != 100_000 {
		t.Fatalf("queued intermediate snapshots: %v", values)
	}
	pump.offer(Progress{Sequence: 100_001})
	if pump.latest != nil {
		t.Fatal("progress accepted after result")
	}
}
func TestProgressCanceledPumpEmitsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pump := startProgressPump(ctx, func(Progress) { t.Error("progress after cancel") })
	pump.offer(Progress{Sequence: 1})
	pump.finish()
}
