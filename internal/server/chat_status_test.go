package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"github.com/gorilla/websocket"
)

// A page that connects mid-turn learns how long each running thread's turn has
// run, so its elapsed counter does not restart from zero. Threads run side by
// side up to max_parallel_chats; an account switch holds them all.
func TestChatRoomStatusReportsRunningThreads(t *testing.T) {
	r := &chatRoom{conns: map[*connWriter]bool{}}
	if st := r.runningStatus(); len(st) != 0 || r.state() != "idle" {
		t.Fatalf("idle room: %v", st)
	}
	a, b := r.turn("a"), r.turn("b")
	if err := a.begin(); err != nil {
		t.Fatal(err)
	}
	if err := a.begin(); err != errChatBusy {
		t.Fatal("one thread ran two turns", err)
	}
	if err := b.begin(); err != nil {
		t.Fatal("a second thread waited for the first", err)
	}
	st := r.runningStatus()
	if len(st) != 2 || st["a"] == nil || st["b"] == nil || r.state() != "running" || !r.threadRunning("b") || r.threadRunning("c") {
		t.Fatalf("running room: %v", st)
	}
	if ms := st["a"].(map[string]any)["elapsed_ms"].(int64); ms < 0 || ms > 5000 {
		t.Fatalf("elapsed %d", ms)
	}
	if err := r.beginExclusive(); err != errChatBusy {
		t.Fatal("account switch while threads run", err)
	}
	for i := 2; i < config.DefaultMaxParallelChats; i++ {
		if err := r.turn(string(rune('c' + i))).begin(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.turn("z").begin(); err != errChatParallel {
		t.Fatal("ran past the parallel limit", err)
	}
	// Deleting waits for the thread's own turn only.
	if err := a.hold(); err != errChatBusy {
		t.Fatal("deleted a running thread", err)
	}
	idle := r.turn("idle")
	if err := idle.hold(); err != nil {
		t.Fatal(err)
	}
	if err := idle.begin(); err != errChatBusy {
		t.Fatal("a held thread started a turn", err)
	}
	idle.unhold()
	for _, tt := range r.runningLocked() {
		tt.release()
	}
	if st := r.runningStatus(); len(st) != 0 || r.state() != "idle" {
		t.Fatalf("released room: %v", st)
	}
	if err := r.beginExclusive(); err != nil {
		t.Fatal(err)
	}
	if err := a.begin(); err != errChatBusy {
		t.Fatal("a turn started during an account switch", err)
	}
	r.endExclusive()
	if err := a.begin(); err != nil {
		t.Fatal(err)
	}
	a.release()
}

// Events of a thread carry its id, so pages showing another thread file them
// under it instead of rendering them.
func TestThreadTaggedEvents(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{map[string]any{"type": "status", "state": "running"}, `{"thread":"t1","state":"running","type":"status"}`},
		{struct{}{}, `{"thread":"t1"}`},
		{[]int{1}, `[1]`},
	} {
		raw, err := json.Marshal(threadTagged{"t1", tc.value})
		if err != nil || string(raw) != tc.want {
			t.Fatalf("%v: %s %v", tc.value, raw, err)
		}
	}
}

// A turn that first starts its stopped workspace says so: pages hear the phase
// begin and end, one that connects meanwhile gets it with the running status,
// and the next turn starts without it.
func TestChatRoomTellsPagesTheWorkspaceIsStarting(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	room := f.s.chat.room(f.sess.ID).activeTurn(t)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	dial := func() *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/sessions/"+f.sess.ID+"/chat", http.Header{"Authorization": []string{"Bearer alice"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		return conn
	}
	status := func(conn *websocket.Conn) map[string]any {
		t.Helper()
		for {
			var m map[string]any
			if err := conn.ReadJSON(&m); err != nil {
				t.Fatal(err)
			}
			if m["type"] == "status" {
				return m
			}
		}
	}
	room.setPhase(chatPhaseStarting) // no turn, nothing to say
	if !room.tryBegin() {
		t.Fatal("cannot begin fixture turn")
	}
	early := dial()
	if m := status(early); m["state"] != "running" || m["phase"] != nil {
		t.Fatalf("on connect: %v", m)
	}
	room.setPhase(chatPhaseStarting)
	if m := status(early); m["state"] != "running" || m["phase"] != chatPhaseStarting {
		t.Fatalf("starting: %v", m)
	}
	late := dial()
	if m := status(late); m["state"] != "running" || m["phase"] != chatPhaseStarting || m["elapsed_ms"] == nil {
		t.Fatalf("joined while starting: %v", m)
	}
	room.setPhase("")
	for _, conn := range []*websocket.Conn{early, late} {
		if m := status(conn); m["state"] != "running" || m["phase"] != "" {
			t.Fatalf("started: %v", m)
		}
	}
	room.setPhase(chatPhaseStarting)
	room.release()
	if !room.tryBegin() {
		t.Fatal("cannot begin the next turn")
	}
	defer room.release()
	if m := status(dial()); m["state"] != "running" || m["phase"] != nil {
		t.Fatalf("the next turn inherited the phase: %v", m)
	}
}
