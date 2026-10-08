package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func switchAccountServer(t *testing.T) (*Server, store.Session) {
	t.Helper()
	s, sess := newTestServer(t)
	s.cfg.Accounts = []config.Account{
		{ID: "previous", Type: config.AgentClaude, Label: "Previous"},
		{ID: "next", Type: config.AgentClaude, Label: "Next"},
		{ID: "codex", Type: config.AgentCodex, Label: "Codex"},
		{ID: "private", Type: config.AgentClaude, Label: "Private", Access: &config.AccountAccess{Mode: "users", Users: []string{"bob"}}},
	}
	s.chat = newChatManager(s)
	sess.Agent, sess.AccountID, sess.Status = config.AgentClaude, "previous", store.StatusStopped
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	return s, sess
}

func switchAccount(s *Server, sess store.Session, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/sessions/"+sess.ID+"/account", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSwitchAccount(w, req, sess)
	return w
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var p apiProblem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %s", w.Body.String())
	}
	return p.Code
}

func TestSwitchAccountRejectsForeignAndCrossAgentAccounts(t *testing.T) {
	s, sess := switchAccountServer(t)
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{}`, "invalid_request", 400},
		{`{"account_id":"missing"}`, "invalid_request", 400},
		{`{"account_id":"codex"}`, "invalid_request", 400},
		{`{"account_id":"private"}`, "account_access_denied", 403},
	} {
		w := switchAccount(s, sess, tc.body)
		if w.Code != tc.status || problemCode(t, w) != tc.code {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	if cur, _ := s.store.Get(sess.ID); cur.AccountID != "previous" {
		t.Fatalf("account changed by a rejected request: %q", cur.AccountID)
	}
}

func TestSwitchAccountWaitsForRunningTurnAndImport(t *testing.T) {
	s, sess := switchAccountServer(t)
	room := s.chat.room(sess.ID)
	if err := room.begin(); err != nil {
		t.Fatal(err)
	}
	w := switchAccount(s, sess, `{"account_id":"next"}`)
	room.end()
	if w.Code != http.StatusConflict || problemCode(t, w) != "chat_busy" {
		t.Fatalf("running turn: %d %s", w.Code, w.Body.String())
	}
	if !s.beginCreationWork(sess.ID) {
		t.Fatal("creation work reserved")
	}
	w = switchAccount(s, sess, `{"account_id":"next"}`)
	s.endCreationWork(sess.ID)
	if w.Code != http.StatusConflict || problemCode(t, w) != "import_pending" {
		t.Fatalf("running import: %d %s", w.Code, w.Body.String())
	}
	if cur, _ := s.store.Get(sess.ID); cur.AccountID != "previous" {
		t.Fatalf("account changed while busy: %q", cur.AccountID)
	}
}

func TestSwitchAccountRebindsAndReleasesReservations(t *testing.T) {
	s, sess := switchAccountServer(t)
	w := switchAccount(s, sess, `{"account_id":"next"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}
	var view sessionView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.AccountID != "next" || view.AccountLabel != "Next" || view.Status != store.StatusStopped {
		t.Fatalf("view = %+v", view)
	}
	// The room and import slot are free again once the switch returns.
	if err := s.chat.room(sess.ID).begin(); err != nil {
		t.Fatalf("chat room left reserved: %v", err)
	}
	s.chat.room(sess.ID).end()
	if !s.beginCreationWork(sess.ID) {
		t.Fatal("import slot left reserved")
	}
	s.endCreationWork(sess.ID)
}
