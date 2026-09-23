package server

import (
	"agentbox/internal/tunnel"
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
	"github.com/gorilla/websocket"
)

func closeTestServer(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownWaitsForAccountingBeforeClosingStore(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := s.store.Grant("alice", 1000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	started := make(chan struct{})
	s.spawn(func() {
		close(started)
		<-s.workContext().Done()
		saved <- s.store.InsertUsage(store.UsageEvent{User: "alice", SessionID: "last", TurnID: "turn", Agent: "claude", Kind: store.UsageKindChat, CostMicroUSD: 350})
	})
	<-started
	closeTestServer(t, s)
	if err := <-saved; err != nil {
		t.Fatalf("store closed before task flushed: %v", err)
	}
	if err := s.store.Put(store.Session{ID: "late", User: "alice"}); err == nil {
		t.Fatal("store left open")
	}
	if s.spawn(func() { t.Error("task admitted after close") }) {
		t.Fatal("task admitted after close")
	}
	late := false
	_, ok := s.track(func() { late = true })
	if ok || !late {
		t.Fatal("late connection escaped shutdown")
	}
	w := httptest.NewRecorder()
	s.admit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request admitted after close") })).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatalf("late request: %d", w.Code)
	}
	closeTestServer(t, s) // idempotent; dependencies not closed twice
	reopened, err := store.Open(filepath.Join(s.cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows := reopened.ListUsage(store.UsageFilter{User: "alice"})
	q, ok := reopened.GetQuota("alice")
	if len(rows) != 1 || rows[0].CostMicroUSD != 350 || !ok || q.BalanceMicroUSD != 650 {
		t.Fatalf("shutdown settlement lost: rows=%+v quota=%+v", rows, q)
	}
}

func TestShutdownDeadlineKeepsStoreUntilTaskFinishes(t *testing.T) {
	s, _ := newTestServer(t)
	release := make(chan struct{})
	s.spawn(func() { <-release })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if err := s.store.Put(store.Session{ID: "pending", User: "alice"}); err != nil {
		t.Fatalf("dependency closed beneath pending work: %v", err)
	}
	close(release)
	closeTestServer(t, s)
}

func TestShutdownClosesUpgradedChatAndIdleTCP(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	srv := httptest.NewServer(s.admit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleChatWS(w, r, sess) })))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, _, err = conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	s.spawn(func() { s.serveConnections(ln, func(c net.Conn) { close(accepted); _, _ = io.Copy(io.Discard, c) }) })
	tcp, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("not accepted")
	}
	closeTestServer(t, s)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = conn.ReadMessage(); err == nil {
		t.Fatal("chat websocket left open")
	}
	tcp.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = tcp.Read(make([]byte, 1)); err == nil {
		t.Fatal("TCP stream left open")
	}
	if _, err = net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("listener left open")
	}
}

func TestShutdownStopsPeriodicWorkersImmediately(t *testing.T) {
	s, _ := newTestServer(t)
	for _, fn := range []func(){s.imageJanitor, s.credSyncLoop, s.idleReaper, s.termUsageLoop, s.termWatchLoop, s.tokenJanitor} {
		s.spawn(fn)
	}
	// No need to wait for hour-long timers: cancellation must wake every worker.
	closeTestServer(t, s)
}

func TestShutdownClosesProxyCONNECTAndTunnelSession(t *testing.T) {
	upstream := startUpstreamSOCKS(t, "test-user", "test-pass")
	s := bridgeTestServer(t, upstream, "test-user", "test-pass")
	s.tunnels = newTunnelHub()
	wireLink(t, s.tunnels, "alice", tunnel.Whitelist{})
	session := s.tunnels.session("alice")
	proxyURL := startBridge(t, s)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetDone := make(chan struct{})
	go func() {
		defer close(targetDone)
		c, err := target.Accept()
		if err == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	userpass := base64.StdEncoding.EncodeToString([]byte("claude-1:" + s.proxySecret("claude-1")))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target.Addr(), target.Addr(), userpass)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	closeTestServer(t, s)
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("yamux session left open")
	}
	select {
	case <-targetDone:
	case <-time.After(time.Second):
		t.Fatal("proxy upstream left open")
	}
	if _, err = conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("CONNECT client left open")
	}
}

func TestShutdownAllowsProtocolInterruptToFinishBeforeCancellation(t *testing.T) {
	s, _ := newTestServer(t)
	s.chat = newChatManager(s)
	room := s.chat.room("synthetic-turn")
	if !room.tryBegin() {
		t.Fatal("cannot begin fixture turn")
	}
	interrupted := make(chan struct{})
	room.setStop(func() { close(interrupted) })
	observed := make(chan error, 1)
	s.spawn(func() {
		defer room.end()
		<-interrupted
		// An app-server interrupt must get a chance to finish and emit its final
		// usage event before context cancellation tears down the stream.
		observed <- s.workContext().Err()
	})
	closeTestServer(t, s)
	if err := <-observed; err != nil {
		t.Fatalf("stream canceled before protocol interrupt could settle: %v", err)
	}
}
