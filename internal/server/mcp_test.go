package server

import (
	"agentbox/internal/store"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPManagementIsolationMaskAndRevision(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	request := func(method, user, body string) *httptest.ResponseRecorder {
		r := asUser(httptest.NewRequest(method, "/api/mcp/demo", strings.NewReader(body)), user, store.RoleUser)
		r.SetPathValue("name", "demo")
		w := httptest.NewRecorder()
		s.handleMCPUser(w, r)
		return w
	}
	payload := `{"revision":0,"entry":{"config":{"type":"http","url":"https://example.com/mcp","headers":{"Authorization":"Bearer synthetic-secret"}}}}`
	if w := request("PUT", "alice", payload); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("PUT", "alice", payload); w.Code != 409 {
		t.Fatal("stale write", w.Code)
	}
	w := request("GET", "alice", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-secret") || !strings.Contains(w.Body.String(), "KEEP_SECRET") {
		t.Fatal(w.Body.String())
	}
	w = request("GET", "bob", "")
	if strings.Contains(w.Body.String(), "demo") {
		t.Fatal("cross-user config read")
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		r := asUser(httptest.NewRequest(method, "/api/sessions/s1/mcp", nil), "bob", store.RoleAdmin)
		r.SetPathValue("id", "s1")
		w = httptest.NewRecorder()
		s.withSession(s.handleMCPSession).ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("cross-user workspace %s: %d", method, w.Code)
		}
	}
	w = request("PUT", "alice", `{"revision":1,"entry":{"config":{"type":"http","url":"https://example.com/mcp","headers":{"Authorization":"__AGENTBOX_KEEP_SECRET__"}}}}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	d, err := s.mcpService().Definition("alice", "s1", "demo")
	if err != nil || d.Headers["Authorization"] != "Bearer synthetic-secret" {
		t.Fatal("secret not retained", err)
	}
}
func TestMCPRejectsInvalidBodiesAndUnsupportedAgent(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "codex"
	w := httptest.NewRecorder()
	s.handleMCPSession(w, httptest.NewRequest("GET", "/", nil), sess)
	if w.Code != 400 {
		t.Fatal("codex accepted")
	}
	for _, body := range []string{`{} {}`, `{"unknown":1}`, strings.Repeat("x", 150<<10)} {
		r := asUser(httptest.NewRequest("PUT", "/api/mcp/demo", strings.NewReader(body)), "alice", store.RoleUser)
		r.SetPathValue("name", "demo")
		w = httptest.NewRecorder()
		s.handleMCPUser(w, r)
		if w.Code != 400 {
			t.Fatalf("bad request accepted: %d", w.Code)
		}
	}
	// Malformed child output is bounded and never turns into an error containing credentials.
	b := &mcpOutput{}
	secret := strings.Repeat("x", (1<<20)+100)
	_, _ = b.Write([]byte(secret))
	if !b.overflow || b.Len() != 1<<20 {
		t.Fatal("unbounded helper output")
	}
	raw, _ := json.Marshal(map[string]any{"revision": 0, "mcpServers": map[string]any{"ok": map[string]any{"command": "node"}, "bad": map[string]any{"type": "sse", "url": "https://example.com"}}})
	r := asUser(httptest.NewRequest("POST", "/api/mcp/import", strings.NewReader(string(raw))), "alice", store.RoleUser)
	w = httptest.NewRecorder()
	s.handleMCPUserImport(w, r)
	if w.Code != 400 {
		t.Fatal("unsupported import accepted")
	}
	view, err := s.mcpService().View("alice", "")
	if err != nil || len(view.Items) > 0 {
		t.Fatal("partial import persisted")
	}
}

func TestMCPCheckAuthorizationAndQuotaBeforeExecution(t *testing.T) {
	s, sess := accessTestServer(t)
	req := accessRequest("alice", "POST", "/api/sessions/s1/mcp/demo/check", "")
	req.SetPathValue("name", "demo")
	w := httptest.NewRecorder()
	s.handleMCPCheck(w, req, sess)
	if w.Code != 403 {
		t.Fatalf("revoked account reached Docker: %d", w.Code)
	}
	sess.AccountID = "shared"
	if _, err := s.store.SetQuotaEnforced(sess.User, true); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleMCPCheck(w, req, sess)
	if w.Code != 403 {
		t.Fatalf("empty quota reached Docker: %d", w.Code)
	}
}
func TestMCPSyncDeferredDuringActiveTurn(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	s.chat = newChatManager(s)
	room := s.chat.room(sess.ID)
	room.running = true
	// An invalid data root would fail any sync; active turns must skip it.
	s.cfg.DataDir = "/does-not-exist-agentbox-mcp-test"
	if err := s.syncMCPOnStart(t.Context(), sess); err != nil {
		t.Fatal("sync ran during active turn", err)
	}
	room.running = false
	if err := s.syncMCPOnStart(t.Context(), sess); err == nil {
		t.Fatal("idle sync was skipped")
	}
}
