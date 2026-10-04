package syncproto

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseOverlapAndIdentityFencing(t *testing.T) {
	l := NewLeases()
	lease, err := l.Acquire("w", "p", "src", "desktop-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".", "src", "src/nested"} {
		if _, err = l.Acquire("w", "other", dir, "desktop-b"); !errors.Is(err, ErrLeaseBusy) {
			t.Fatalf("overlap %s: %v", dir, err)
		}
	}
	if _, err = l.Acquire("w", "sibling", "src-other", "desktop-b"); err != nil {
		t.Fatal("unrelated sibling blocked", err)
	}
	if _, err = l.Acquire("other-workspace", "p", "src", "desktop-b"); err != nil {
		t.Fatal("other workspace blocked", err)
	}
	for _, bad := range []struct{ device, token, generation string }{{"desktop-b", lease.Token, lease.Generation}, {lease.Device, "wrong", lease.Generation}, {lease.Device, lease.Token, "old-generation"}} {
		if _, err = l.Check("w", "p", "src", bad.device, bad.token, bad.generation); !errors.Is(err, ErrLeaseExpired) {
			t.Fatal("forged holder accepted", err)
		}
	}
	if _, err = l.Check("w", "p", "elsewhere", lease.Device, lease.Token, lease.Generation); !errors.Is(err, ErrLeaseExpired) {
		t.Fatal("changed mapping accepted")
	}
	if err = l.Release("w", "p", "src", lease.Device, lease.Token, lease.Generation); err != nil {
		t.Fatal(err)
	}
	if l.Active("w", "p") {
		t.Fatal("release retained grant")
	}
}

func TestExpiredLeaseCannotRenewOrReleaseSuccessor(t *testing.T) {
	l := NewLeases()
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	first, err := l.Acquire("w", "p", ".", "device")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	renewed, err := l.Renew("w", "p", ".", first.Device, first.Token, first.Generation)
	if err != nil || !renewed.Expires.Equal(now.Add(LeaseTTL)) {
		t.Fatal("renewal", err)
	}
	now = renewed.Expires // Equality is expired, not one final allowed write.
	if _, err = l.Check("w", "p", ".", first.Device, first.Token, first.Generation); !errors.Is(err, ErrLeaseExpired) {
		t.Fatal(err)
	}
	second, err := l.Acquire("w", "p", ".", "device")
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.Generation == second.Generation {
		t.Fatal("grant generation reused")
	}
	if err = l.Release("w", "p", ".", first.Device, first.Token, first.Generation); !errors.Is(err, ErrLeaseExpired) {
		t.Fatal("stale release accepted", err)
	}
	if _, err = l.Renew("w", "p", ".", first.Device, first.Token, first.Generation); !errors.Is(err, ErrLeaseExpired) {
		t.Fatal("stale renewal accepted", err)
	}
	if !l.Active("w", "p") {
		t.Fatal("stale request removed current lease")
	}
	if _, err = NewLeases().Check("w", "p", ".", second.Device, second.Token, second.Generation); !errors.Is(err, ErrLeaseExpired) {
		t.Fatal("lease survived restart")
	}
}

func TestOnlyOneConcurrentWriterAcquiresRoot(t *testing.T) {
	l := NewLeases()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Acquire("w", "p", ".", "device"); err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrLeaseBusy) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("got %d writers", accepted.Load())
	}
}
