package workspace

import (
	"testing"
	"time"
)

func TestActivityReapable(t *testing.T) {
	a := NewActivity()

	// Unknown session: seeded and spared on first check (boot-survivor grace).
	if a.Reapable("s1", time.Hour) {
		t.Fatal("unknown session should not be reapable on first check")
	}
	// Now known but freshly seeded — still not idle long enough.
	if a.Reapable("s1", time.Hour) {
		t.Fatal("freshly seeded session should not be reapable")
	}

	// Force the record stale, then it becomes reapable.
	a.mu.Lock()
	a.seen["s1"].lastSeen = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()
	if !a.Reapable("s1", time.Hour) {
		t.Fatal("stale idle session should be reapable")
	}

	// touch resets the countdown.
	a.Touch("s1")
	if a.Reapable("s1", time.Hour) {
		t.Fatal("touched session should not be reapable")
	}
}

func TestActivityHoldPreventsReap(t *testing.T) {
	a := NewActivity()
	a.Hold("s1")

	// Even with a stale timestamp, a held session is never reaped.
	a.mu.Lock()
	a.seen["s1"].lastSeen = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()
	if a.Reapable("s1", time.Hour) {
		t.Fatal("held session must not be reapable")
	}

	// Releasing the last hold makes it eligible again (timestamp still stale
	// from before, but release bumps lastSeen, so force it stale once more).
	a.Release("s1")
	a.mu.Lock()
	a.seen["s1"].lastSeen = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()
	if !a.Reapable("s1", time.Hour) {
		t.Fatal("released session should be reapable once idle")
	}
}

func TestActivityHoldBalance(t *testing.T) {
	a := NewActivity()
	a.Hold("s1")
	a.Hold("s1") // two concurrent uses (e.g. terminal + chat turn)
	a.Release("s1")

	a.mu.Lock()
	a.seen["s1"].lastSeen = time.Now().Add(-2 * time.Hour)
	holds := a.seen["s1"].holds
	a.mu.Unlock()
	if holds != 1 {
		t.Fatalf("holds = %d, want 1", holds)
	}
	if a.Reapable("s1", time.Hour) {
		t.Fatal("still-held session must not be reapable")
	}
}
