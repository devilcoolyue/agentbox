package chat

import (
	"sync/atomic"
	"testing"
	"time"
)

// Waiting for an answer does not use up the turn's own time limit.
func TestTurnClockPausesWhileWaiting(t *testing.T) {
	var fired atomic.Bool
	c := newTurnClock(80*time.Millisecond, func() { fired.Store(true) })
	defer c.stop()
	time.Sleep(30 * time.Millisecond)
	c.pause()
	c.pause() // two questions at once
	time.Sleep(120 * time.Millisecond)
	c.resume()
	if fired.Load() {
		t.Fatal("expired while a question waited")
	}
	c.resume()
	time.Sleep(20 * time.Millisecond)
	if fired.Load() {
		t.Fatal("wait counted against the limit")
	}
	deadline := time.Now().Add(time.Second)
	for !fired.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !fired.Load() {
		t.Fatal("limit never expired after resuming")
	}
}
