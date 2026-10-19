package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/store"
)

func TestUsageLiveHubScopesAndCoalesces(t *testing.T) {
	h := newUsageLiveHub()
	alice, root := h.subscribe("alice", false), h.subscribe("root", true)
	h.publish([]store.UsageWrite{{User: "bob", SessionID: "b1"}})
	select {
	case <-alice.signal:
		t.Fatal("alice was told about bob's workspace")
	default:
	}
	h.publish([]store.UsageWrite{{User: "alice", SessionID: "a2"}, {User: "alice", SessionID: "a1"}})
	h.publish([]store.UsageWrite{{User: "alice", SessionID: "a1"}})
	if len(alice.signal) != 1 || len(root.signal) != 1 {
		t.Fatal("writes were not coalesced into one pending signal")
	}
	if got := alice.take(); strings.Join(got, ",") != "a1,a2" {
		t.Fatalf("alice sessions = %v", got)
	}
	if got := root.take(); strings.Join(got, ",") != "a1,a2,b1" {
		t.Fatalf("admin sessions = %v", got)
	}
	if got := alice.take(); got == nil || len(got) != 0 {
		t.Fatalf("take did not reset: %v", got)
	}
	<-alice.signal
	<-root.signal

	// 角色降级后只收自己的；一条通知列不下的空间不逐个列出。
	root.setAdmin(false)
	h.publish([]store.UsageWrite{{User: "bob", SessionID: "b1"}})
	if len(root.signal) != 0 || len(root.sessions) != 0 {
		t.Fatal("demoted admin still collects other users")
	}
	var many []store.UsageWrite
	for i := 0; i <= usageLiveMaxSessions; i++ {
		many = append(many, store.UsageWrite{User: "alice", SessionID: fmt.Sprint("s", i)})
	}
	h.publish(many)
	if got := alice.take(); got != nil {
		t.Fatalf("overflow listed %d sessions", len(got))
	}
	<-alice.signal
	h.unsubscribe(alice)
	h.publish([]store.UsageWrite{{User: "alice", SessionID: "a1"}})
	if len(alice.signal) != 0 {
		t.Fatal("unsubscribed connection still notified")
	}
}

// 用量写入提交后，开着推送连接的页面收到只含空间 ID 的通知；普通用户收不到
// 别人的，管理员都收得到。没登录的连接升级不上来。
func TestUsageLiveWebSocketDeliversCommittedWrites(t *testing.T) {
	s, _ := accessTestServer(t)
	s.usageLive = newUsageLiveHub()
	s.store.OnUsageWritten(s.usageLive.publish)
	for _, user := range []string{"alice", "root"} {
		if err := s.store.CreateToken("live-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/usage/live"
	if _, resp, err := websocket.DefaultDialer.Dial(base, nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dial: %v %v", resp, err)
	}
	dial := func(user string) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(base+"?token=live-"+user, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	alice, root := dial("alice"), dial("root")
	// 订阅在升级之后才登记；等它们都进了推送中心再写。
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.usageLive.mu.Lock()
		n := len(s.usageLive.subs)
		s.usageLive.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscribers = %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	read := func(conn *websocket.Conn, wait time.Duration) (usageLiveNotice, error) {
		var n usageLiveNotice
		conn.SetReadDeadline(time.Now().Add(wait))
		err := conn.ReadJSON(&n)
		return n, err
	}

	write := func(user, session string) {
		t.Helper()
		if err := s.store.InsertUsage(store.UsageEvent{User: user, SessionID: session, Agent: "claude", Model: "m", InputTokens: 1}); err != nil {
			t.Fatal(err)
		}
	}
	write("bob", "b1")
	if n, err := read(root, 3*time.Second); err != nil || n.Type != "usage" || strings.Join(n.Sessions, ",") != "b1" {
		t.Fatalf("admin notice = %+v, %v", n, err)
	}
	write("alice", "s1")
	for name, conn := range map[string]*websocket.Conn{"alice": alice, "root": root} {
		if n, err := read(conn, 3*time.Second); err != nil || strings.Join(n.Sessions, ",") != "s1" {
			t.Fatalf("%s notice = %+v, %v", name, n, err)
		}
	}
	// alice 没收到 bob 那条：读到的第一条就是自己的 s1，之后也没有别的。
	if n, err := read(alice, 2*usageLiveDebounce); err == nil {
		t.Fatalf("alice got an extra notice %+v", n)
	}
}
