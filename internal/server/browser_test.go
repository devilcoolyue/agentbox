package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/dockerx"
)

func TestBrowserURL(t *testing.T) {
	for _, raw := range []string{"", "https://claude.ai/", "http://localhost:3000/?q=hello", "https://example.org/a#b"} {
		if !validBrowserURL(raw) {
			t.Errorf("rejected %q", raw)
		}
	}
	for _, raw := range []string{"javascript:alert(1)", "file:///etc/passwd", "--no-sandbox", "https://user:secret@example.com", "https://", "https://example.com\n", strings.Repeat("a", 8193)} {
		if validBrowserURL(raw) {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestBrowserAccessBeforeDocker(t *testing.T) {
	s, sess := accessTestServer(t)
	// alice owns the workspace, but the account grants only bob. Docker is nil:
	// all routes must reject before touching the container or starting anything.
	for _, method := range []string{"GET", "POST", "DELETE", "WS"} {
		w := httptest.NewRecorder()
		r := accessRequest("alice", "GET", "/browser", "")
		if method == "WS" {
			s.handleBrowserWS(w, r, sess)
		} else {
			r.Method = method
			s.handleBrowser(w, r, sess)
		}
		if w.Code != 403 {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body.String())
		}
	}
	// A different user cannot reach even the status handler through withSession.
	w := httptest.NewRecorder()
	r := accessRequest("bob", "GET", "/browser", "")
	r.SetPathValue("id", sess.ID)
	s.withSession(s.handleBrowser).ServeHTTP(w, r)
	if w.Code != 404 && w.Code != 403 {
		t.Fatalf("cross-user: %d", w.Code)
	}
}

func TestBrowserNetworkFailsClosedAndRedacts(t *testing.T) {
	s, sess := accessTestServer(t)
	sess.AccountID = "shared"
	env, oldKey, err := s.browserEnv(sess)
	if err != nil || len(env) != 1 {
		t.Fatalf("direct: %v %v", env, err)
	}
	s.cfg.Accounts = []config.Account{{ID: "shared", Type: "claude", ProxyID: "missing"}}
	if _, _, err = s.browserEnv(sess); err == nil {
		t.Fatal("dangling proxy fell back to direct")
	}
	s.cfg.Proxies = []config.Proxy{{ID: "missing", Scheme: "http", Host: "127.0.0.1", Port: 1234}}
	if _, _, err = s.browserEnv(sess); err == nil {
		t.Fatal("offline bridge accepted")
	}
	s.bridgeUp.Store(true)
	s.cfg.ProxyBridge = config.ProxyBridgeConfig{Bind: "127.0.0.1:1081", Host: "127.0.0.1"}
	env, key, err := s.browserEnv(sess)
	if err != nil || key == oldKey || len(env) != 2 {
		t.Fatalf("proxy: %v %v", env, err)
	}
	if strings.Contains(strings.Join(env, " "), "SECRET") {
		t.Fatal("account env leaked")
	}
	raw, _ := json.Marshal(dockerx.BrowserInfo{NetworkKey: key, Running: true})
	if strings.Contains(string(raw), key) {
		t.Fatal("network key sent to browser")
	}
}
