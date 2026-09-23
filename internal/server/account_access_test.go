package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/gorilla/websocket"
)

func accessTestServer(t *testing.T) (*Server, store.Session) {
	t.Helper()
	s, sess := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"data_dir": s.cfg.DataDir, "auth_token": "test-token-long-enough", "agent_image": "test", "accounts": []config.Account{
		{ID: "shared", Type: config.AgentClaude},
		{ID: "selected", Type: config.AgentClaude, Access: &config.AccountAccess{Mode: "users", Users: []string{"bob"}}, Env: map[string]string{"SECRET": "test-only-secret"}},
		{ID: "admins", Type: config.AgentClaude, Access: &config.AccountAccess{Mode: "admin"}},
	}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	s.cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{{Name: "alice", Role: store.RoleUser}, {Name: "bob", Role: store.RoleUser}, {Name: "root", Role: store.RoleAdmin}} {
		if err := s.store.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	sess.AccountID, sess.Agent = "selected", config.AgentClaude
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.starts = map[string]*sync.Mutex{}
	s.chat = newChatManager(s)
	return s, sess
}

func accessRequest(user, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	role := store.RoleUser
	if user == "root" {
		role = store.RoleAdmin
	}
	return r.WithContext(context.WithValue(r.Context(), ctxUser, store.User{Name: user, Role: role}))
}

func TestAccountsFilterAndRedact(t *testing.T) {
	s, sess := accessTestServer(t)
	for _, tt := range []struct {
		user  string
		count int
	}{{"alice", 1}, {"bob", 2}, {"root", 3}} {
		w := httptest.NewRecorder()
		s.handleAccounts(w, accessRequest(tt.user, "GET", "/accounts", ""))
		var rows []acctView
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != tt.count {
			t.Fatalf("%s: %s", tt.user, w.Body.String())
		}
		for _, a := range rows {
			if tt.user != "root" && (a.Access != nil || a.Env != nil || a.Sessions != 0) {
				t.Fatalf("private data leaked: %+v", a)
			}
		}
	}
	if _, err := s.sessionAccount(sess); err != errAccountAccess {
		t.Fatal(err)
	}
	sess.User = "root"
	if _, err := s.sessionAccount(sess); err != nil {
		t.Fatal(err)
	}
}

func TestAccountAccessRejectsBeforeSideEffects(t *testing.T) {
	s, sess := accessTestServer(t)
	// Docker/activity are nil: these paths must reject before any runtime work.
	w := httptest.NewRecorder()
	s.handleCreateSession(w, accessRequest("alice", "POST", "/sessions", `{"name":"blocked","agent":"claude","account_id":"selected"}`))
	if w.Code != 403 || len(s.store.All()) != 1 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.startSession(t.Context(), sess); err != errAccountAccess {
		t.Fatalf("start: %v", err)
	}
	w = httptest.NewRecorder()
	s.handleStartSession(w, accessRequest("alice", "POST", "/start", ""), sess)
	if w.Code != 403 {
		t.Fatalf("REST start: %d", w.Code)
	}
	if _, _, err := s.prepareGitSession(t.Context(), sess.ID); err != errAccountAccess {
		t.Fatalf("git: %v", err)
	}
	w = httptest.NewRecorder()
	s.handleAccountUsage(w, accessRequest("alice", "GET", "/usage", ""), sess)
	if w.Code != 403 {
		t.Fatalf("account usage: %d", w.Code)
	}
	if env, err := s.execEnv(sess); err != errAccountAccess || len(env) != 0 {
		t.Fatalf("unauthorized env: %v", env)
	}
	// Resource ownership remains separate: revoking account use does not strand files.
	w = httptest.NewRecorder()
	r := accessRequest("alice", "GET", "/sessions/s1/files", "")
	r.SetPathValue("id", sess.ID)
	s.withSession(s.handleFiles).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("files: %d %s", w.Code, w.Body.String())
	}
	r = accessRequest("bob", "GET", "/sessions/s1/files", "")
	r.SetPathValue("id", sess.ID)
	w = httptest.NewRecorder()
	s.withSession(s.handleFiles).ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("foreign files: %d", w.Code)
	}
}

func TestAccountAccessPatchAndLegacyEdit(t *testing.T) {
	s, _ := accessTestServer(t)
	for _, user := range []string{"alice", "root"} {
		if err := s.store.CreateToken("test-token-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(user, body string) *httptest.ResponseRecorder {
		r := accessRequest(user, "PATCH", "/accounts/selected", body)
		r.SetPathValue("id", "selected")
		r.Header.Set("Authorization", "Bearer test-token-"+user)
		w := httptest.NewRecorder()
		s.admin(http.HandlerFunc(s.handleAccountPatch)).ServeHTTP(w, r)
		return w
	}
	if w := patch("alice", `{"access":{"mode":"all"}}`); w.Code != 403 {
		t.Fatalf("non-admin edit: %d", w.Code)
	}
	if w := patch("root", `{"access":{"mode":"users","users":["typo"]}}`); w.Code != 400 {
		t.Fatalf("unknown user: %d", w.Code)
	}
	if w := patch("root", `{"label":"new label"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a, _ := s.cfg.Account("selected")
	if a.CanUse("alice", false) {
		t.Fatal("legacy edit opened account")
	}
	if w := patch("root", `{"access":{"mode":"users","users":["alice"]}}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	reloaded, err := config.Load(s.cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	a, _ = reloaded.Account("selected")
	if !a.CanUse("alice", false) || a.CanUse("bob", false) {
		t.Fatal("access not persisted")
	}
	if w := patch("root", `{"access":{"mode":"all"}}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a, _ = s.cfg.Account("selected")
	if !a.CanUse("bob", false) {
		t.Fatal("cannot restore sharing")
	}
}

func TestAccountAccessTerminalCloseReason(t *testing.T) {
	s, sess := accessTestServer(t)
	conn := dialTerm(t, s, sess)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4004 || !strings.Contains(ce.Text, "无权") {
		t.Fatalf("terminal close: %v", err)
	}
}

func TestAccountAccessExistingChatRechecksEachMessage(t *testing.T) {
	s, sess := accessTestServer(t)
	policy := &config.AccountAccess{Mode: "all"}
	if _, err := s.cfg.UpdateAccount(sess.AccountID, config.AccountPatch{Access: policy}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleChatWS(w, r, sess) }))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var msg map[string]any
	if err = conn.ReadJSON(&msg); err != nil {
		t.Fatal(err)
	} // initial status
	if _, err = s.cfg.UpdateAccount(sess.AccountID, config.AccountPatch{Access: &config.AccountAccess{Mode: "admin"}}); err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteJSON(map[string]string{"type": "user_message", "text": "must not run"}); err != nil {
		t.Fatal(err)
	}
	if err = conn.ReadJSON(&msg); err != nil {
		t.Fatal(err)
	}
	if msg["type"] != "error" || !strings.Contains(msg["error"].(string), "无权") {
		t.Fatalf("chat response: %v", msg)
	}
}

func TestAccountRevocationStopsCredentialSyncBothDirections(t *testing.T) {
	s, sess := accessTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(time.Hour).UnixMilli())
	home := filepath.Join(s.homeDir(sess), ".claude")
	if err := os.MkdirAll(home, 0755); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(home, ".credentials.json")
	poolFile := filepath.Join(acct.CredentialsDir, ".credentials.json")
	old := time.Now().Add(-time.Hour)
	if err := os.WriteFile(homeFile, []byte(`{"refreshToken":"home-old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(homeFile, old, old); err != nil {
		t.Fatal(err)
	}
	// Stale caller holds shared policy; the config now denies this owner.
	s.cfg.Accounts[0].Access = &config.AccountAccess{Mode: "admin"}
	poolBefore, _ := os.ReadFile(poolFile)
	s.syncRotatingCred(acct, sess)
	raw, _ := os.ReadFile(homeFile)
	if !strings.Contains(string(raw), "home-old") {
		t.Fatal("revoked home received pool credentials")
	}
	if err := os.Chtimes(poolFile, old.Add(-time.Hour), old.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.syncRotatingCred(acct, sess)
	raw, _ = os.ReadFile(poolFile)
	if string(raw) != string(poolBefore) {
		t.Fatal("revoked home updated pool")
	}
	s.cfg.Accounts[0].Access = &config.AccountAccess{Mode: "all"}
	s.syncRotatingCred(acct, sess)
	raw, _ = os.ReadFile(poolFile)
	if !strings.Contains(string(raw), "home-old") {
		t.Fatal("authorized sync no longer works")
	}
}
