package server

import (
	"testing"
	"time"
)

// A page that connects mid-turn learns when the turn began, so its elapsed
// counter does not restart from zero.
func TestChatRoomStatusReportsTurnStart(t *testing.T) {
	r := &chatRoom{conns: map[*connWriter]bool{}}
	if state, at := r.status(); state != "idle" || !at.IsZero() {
		t.Fatalf("idle room: %s %v", state, at)
	}
	before := time.Now()
	if err := r.begin(); err != nil {
		t.Fatal(err)
	}
	state, at := r.status()
	if state != "running" || at.Before(before) || at.After(time.Now()) {
		t.Fatalf("running room: %s %v", state, at)
	}
	r.release()
	if state, at := r.status(); state != "idle" || !at.IsZero() {
		t.Fatalf("released room: %s %v", state, at)
	}
}
