package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"agentbox/internal/workspace"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

type problemLog struct {
	sync.Mutex
	bytes.Buffer
}

func (b *problemLog) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *problemLog) text() string { b.Lock(); defer b.Unlock(); return b.Buffer.String() }
func captureProblemLogs(t *testing.T) *problemLog {
	t.Helper()
	b := &problemLog{}
	previous := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(previous) })
	return b
}

func assertProblem(t *testing.T, raw []byte, code string) apiProblem {
	t.Helper()
	var p apiProblem
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != code || p.Error == "" || p.Hint == "" || p.Action == "" || !operationIDPattern.MatchString(p.OperationID) {
		t.Fatalf("invalid error response: %s", raw)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["retryable"].(bool); !ok {
		t.Fatal("missing retryable boolean")
	}
	return p
}

func TestProblemsCatalogContractV1(t *testing.T) {
	raw, err := os.ReadFile("testdata/problems-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version int
		Errors  []struct {
			Code, Error, Hint, Action string
			Retryable                 bool
			Status                    int `json:"http_status"`
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 1 || len(fixture.Errors) != len(problemCatalog) {
		t.Fatal("error contract scope changed")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Errors {
		want := problemDefinition{row.Status, row.Error, row.Hint, row.Action, row.Retryable}
		if got, ok := problemCatalog[row.Code]; !ok || got != want || seen[row.Code] {
			t.Fatalf("contract mismatch for %s", row.Code)
		}
		seen[row.Code] = true
	}
}

func TestProblemsClassificationAndRedaction(t *testing.T) {
	logs := captureProblemLogs(t)
	const secret = "synthetic-token /private/operator/config.json user-not-authorized"
	cases := []struct {
		cause error
		code  string
	}{
		{client.ErrorConnectionFailed(secret), "docker_unavailable"},
		{fmt.Errorf("%w: %s", dockerx.ErrImageMissing, secret), "agent_image_missing"},
		{fmt.Errorf("%w: %s", errAccountAccess, secret), "account_access_denied"},
		{fmt.Errorf("%w: %s", workspace.ErrDiskSpace, secret), "storage_full"},
		{&os.PathError{Op: "write", Path: secret, Err: syscall.ENOSPC}, "storage_full"},
		{&os.PathError{Op: "open", Path: secret, Err: syscall.EACCES}, "storage_permission_denied"},
		{fmt.Errorf("%w: %s", workspace.ErrCredentials, secret), "account_credentials_unavailable"},
		{context.DeadlineExceeded, "operation_timed_out"},
		{errors.New(secret), "workspace_start_failed"},
	}
	ids := map[string]bool{}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/sessions/synthetic/start?token=synthetic-query-secret", nil)
		req.Header.Set("X-Agentbox-Operation-ID", "injected-secret")
		operationHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, r, "session.start", classifyProblem(fmt.Errorf("wrapped: %w", tc.cause), "workspace_start_failed"))
		})).ServeHTTP(w, req)
		p := assertProblem(t, w.Body.Bytes(), tc.code)
		if w.Code != problemCatalog[tc.code].status || w.Header().Get("X-Agentbox-Operation-ID") != p.OperationID || ids[p.OperationID] {
			t.Fatal("status or correlation mismatch")
		}
		ids[p.OperationID] = true
		if !strings.Contains(logs.text(), "operation_id="+p.OperationID+" operation=session.start code="+tc.code) {
			t.Fatal("cannot correlate error with log")
		}
		if strings.Contains(w.Body.String()+logs.text(), secret) || strings.Contains(logs.text(), "synthetic-query-secret") || strings.Contains(logs.text(), "injected-secret") {
			t.Fatal("raw error/request leaked")
		}
		var legacy struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &legacy); err != nil || legacy.Error == "" {
			t.Fatal("old client no longer sees error")
		}
	}
}

func TestProblemsLoginAndSessionAccess(t *testing.T) {
	s, sess := accessTestServer(t)
	if err := s.store.SetPassword("alice", hashPassword("synthetic-correct-password")); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateToken("synthetic-alice-token", "alice"); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	var previous apiProblem
	for _, name := range []string{"alice", "missing"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"`+name+`","password":"synthetic-wrong"}`))
		handler.ServeHTTP(w, r)
		p := assertProblem(t, w.Body.Bytes(), "invalid_credentials")
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
		p.OperationID = ""
		if name == "missing" && !reflect.DeepEqual(p, previous) {
			t.Fatal("login error enumerates accounts")
		}
		previous = p
	}
	for _, tc := range []struct {
		path, token, code string
		status            int
	}{
		{"/api/me", "expired", "authentication_required", 401},
		{"/api/sessions/unknown", "synthetic-alice-token", "session_not_found", 404},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		handler.ServeHTTP(w, r)
		assertProblem(t, w.Body.Bytes(), tc.code)
		if w.Code != tc.status {
			t.Fatal(w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/sessions/"+sess.ID+"/start", nil)
	r.Header.Set("Authorization", "Bearer synthetic-alice-token")
	handler.ServeHTTP(w, r)
	assertProblem(t, w.Body.Bytes(), "account_access_denied")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestProblemsInvalidLoginAndRateLimit(t *testing.T) {
	s, _ := newTestServer(t)
	logs := captureProblemLogs(t)
	handler := operationHandler(http.HandlerFunc(s.handleLogin))
	for _, body := range []string{"{", strings.Repeat(" ", 16<<10) + `{}`} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/login", strings.NewReader(body)))
		assertProblem(t, w.Body.Bytes(), "invalid_request")
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"synthetic-user","password":"synthetic-private-password"}`))
	for i := 0; i < loginMaxFails; i++ {
		s.logins.fail(clientIP(r))
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	p := assertProblem(t, w.Body.Bytes(), "login_rate_limited")
	if w.Code != 429 || !p.Retryable || p.Action != "wait" {
		t.Fatal("rate limit recovery contract lost")
	}
	if strings.Contains(logs.text(), "synthetic-user") || strings.Contains(logs.text(), "synthetic-private-password") {
		t.Fatal("login inputs leaked")
	}
}

func TestProblemsWebSocketHandshakeAndAdmission(t *testing.T) {
	logs := captureProblemLogs(t)
	s, sess := accessTestServer(t)
	if err := s.store.CreateToken("synthetic-alice-token", "alice"); err != nil {
		t.Fatal(err)
	}
	s.upgrader.CheckOrigin = sameHostOrigin
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	const connectionID = "0123456789abcdef0123456789abcdef"
	url := srv.URL + "/api/sessions/" + sess.ID + "/chat?token=synthetic-alice-token&connection_id=" + connectionID
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	p := assertProblem(t, raw, "websocket_failed")
	if p.OperationID != connectionID || response.Header.Get("X-Agentbox-Operation-ID") != connectionID || !strings.Contains(logs.text(), "operation_id="+connectionID) {
		t.Fatal("handshake correlation lost")
	}
	// Strictly reject a browser's cross-origin request using the same JSON contract.
	_, response, err = websocket.DefaultDialer.Dial(strings.Replace(url, "http:", "ws:", 1), http.Header{"Origin": []string{"https://untrusted.invalid"}})
	if err == nil || response == nil || response.StatusCode != 403 {
		t.Fatal("cross-origin handshake was not rejected")
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	assertProblem(t, raw, "websocket_failed")
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(url, "http:", "ws:", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var message json.RawMessage
	if err := conn.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	var lastID string
	for _, code := range []string{"account_access_denied", "quota_exhausted"} {
		if code == "quota_exhausted" {
			if _, err := s.store.SetQuotaEnforced(sess.User, true); err != nil {
				t.Fatal(err)
			}
		}
		if err := conn.WriteJSON(map[string]string{"type": "user_message", "text": "synthetic-private-prompt"}); err != nil {
			t.Fatal(err)
		}
		if err := conn.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		p := assertProblem(t, message, code)
		if p.OperationID == lastID || p.OperationID == connectionID {
			t.Fatal("message reused operation ID")
		}
		lastID = p.OperationID
		if !strings.Contains(logs.text(), "operation_id="+p.OperationID+" operation=chat.admission code="+code) {
			t.Fatal("admission log not correlated")
		}
		if s.chat.room(sess.ID).state() != "idle" {
			t.Fatal("rejected request started a turn")
		}
	}
	if strings.Contains(logs.text(), "synthetic-private-prompt") || strings.Contains(logs.text(), "synthetic-alice-token") {
		t.Fatal("private request in log")
	}
}

// Exercise real Start admission/seeding. Linux CI runs server tests as root,
// because production must chown session files to UID 1000 (never relax it).
type problemRuntime struct{ err error }

func (d problemRuntime) Running(context.Context, string) (bool, error)         { return false, nil }
func (d problemRuntime) RunningWithMount(context.Context, string, string) bool { return false }
func (d problemRuntime) EnsureRunning(context.Context, store.Session, config.Account, string, string, string) (string, error) {
	return "", d.err
}
func (d problemRuntime) Stop(context.Context, string) error   { return nil }
func (d problemRuntime) Remove(context.Context, string) error { return nil }

func TestProblemsStartupThroughWorkspace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for production UID 1000 seeding; covered by Linux server tests")
	}
	for _, tc := range []struct {
		code  string
		cause error
	}{
		{"docker_unavailable", client.ErrorConnectionFailed("synthetic-private-docker-socket")},
		{"agent_image_missing", fmt.Errorf("%w: synthetic-private-image", dockerx.ErrImageMissing)},
		{"storage_full", nil},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s, sess := accessTestServer(t)
			sess.AccountID = "shared"
			sess.DefaultModel = s.cfg.GetDefaultModel(sess.Agent)
			if err := s.store.Put(sess); err != nil {
				t.Fatal(err)
			}
			if tc.code == "storage_full" {
				s.cfg.Resources.MinFreeBytes = 1 << 62
			}
			s.workspaceOnce.Do(func() {
				s.workspace = workspace.New(s.cfg, s.store, problemRuntime{tc.cause}, s.sessionAccount, func(context.Context, config.Account, store.Session) error { return nil })
			})
			if err := s.workspace.Create(sess); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			operationHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleStartSession(w, r, sess) })).ServeHTTP(w, httptest.NewRequest("POST", "/start", nil))
			p := assertProblem(t, w.Body.Bytes(), tc.code)
			if strings.Contains(p.Error+p.Hint, "synthetic-private") {
				t.Fatal("runtime detail leaked")
			}
			room := s.chat.room(sess.ID)
			// Now use valid defaults so the same startup error is persisted for history.
			room.runTurn("synthetic-private-prompt", "", "")
			history := httptest.NewRecorder()
			s.handleHistory(history, httptest.NewRequest("GET", "/history", nil), sess)
			var result struct {
				Entries []logEntry `json:"entries"`
			}
			if err := json.Unmarshal(history.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Entries) == 0 {
				t.Fatal("startup failure missing from history")
			}
			last := result.Entries[len(result.Entries)-1]
			if last.ProblemDetails == nil || last.Code != tc.code || !operationIDPattern.MatchString(last.OperationID) {
				t.Fatalf("history lost error: %s", history.Body.String())
			}
		})
	}
}
