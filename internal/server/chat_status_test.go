package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"github.com/gorilla/websocket"
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

// A turn that first starts its stopped workspace says so: pages hear the phase
// begin and end, one that connects meanwhile gets it with the running status,
// and the next turn starts without it.
func TestChatRoomTellsPagesTheWorkspaceIsStarting(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	room := f.s.chat.room(f.sess.ID)
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
