package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/diagnostics"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"github.com/gorilla/websocket"
)

func TestDiagnosticsAuthorizedWorkspaceIsReadOnlyAndHasNoInstanceDetails(t *testing.T) {
	s, sess := accessTestServer(t)
	sess.AccountID = "shared"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ANTHROPIC_API_KEY": "private-workspace-key"}
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Env: &env}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.homeDir(sess), 0700); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 || os.Geteuid() == 1000 {
		if err := s.workspaces().Create(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.CreateToken("diag-alice", "alice"); err != nil {
		t.Fatal(err)
	}
	before := s.store.All()
	r := httptest.NewRequest("POST", "/api/sessions/s1/diagnostics", nil)
	r.Header.Set("Authorization", "Bearer diag-alice")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var report diagnostics.Report
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"account_access", "account_configuration", "quota"} {
		if diagnosticCheck(t, report, id).State != diagnostics.Passed {
			t.Fatal("authorized check did not pass", id, w.Body.String())
		}
	}
	if os.Geteuid() == 0 || os.Geteuid() == 1000 {
		if diagnosticCheck(t, report, "workspace_permissions").State != diagnostics.Passed {
			t.Fatal("valid workspace root permissions rejected")
		}
	}
	for _, c := range report.Checks {
		if c.ID == "configuration" || c.ID == "data_permissions" || c.ID == "container_ownership" {
			t.Fatal("instance-only check in workspace report")
		}
	}
	assertDiagnosticWhitelist(t, w.Body.Bytes())
	if strings.Contains(w.Body.String(), "private-workspace-key") || strings.Contains(w.Body.String(), s.cfg.DataDir) {
		t.Fatal("credential/path leaked")
	}
	after := s.store.All()
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) || len(s.store.ListUsage(store.UsageFilter{})) != 0 {
		t.Fatal("diagnosis changed sessions or accounting")
	}
}

func TestDiagnosticsDockerImageAndDiskFailuresAreCorrelated(t *testing.T) {
	s, _ := accessTestServer(t)
	var mode atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			if mode.Load() == 1 {
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"message":"private-engine-secret"}`))
				return
			}
			_, _ = w.Write([]byte("OK"))
			return
		}
		if strings.Contains(r.URL.Path, "/images/") {
			if mode.Load() == 2 {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"message":"private-image-name"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Id":"fixture"}`))
			return
		}
		t.Error("unexpected Docker call", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}))
	defer engine.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(engine.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.45")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	dock, err := dockerx.New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dock.Close()
	s.dock = dock
	logs := captureProblemLogs(t)
	for _, tc := range []struct {
		mode     int32
		id, code string
	}{{1, "docker", "docker_unavailable"}, {2, "agent_image", "image_missing"}, {0, "data_disk", "storage_full"}} {
		mode.Store(tc.mode)
		if tc.id == "data_disk" {
			s.cfg.Resources.MinFreeBytes = 1 << 62
		}
		w := httptest.NewRecorder()
		operationHandler(http.HandlerFunc(s.handleCheckDiagnostics)).ServeHTTP(w, httptest.NewRequest("POST", "/api/diagnostics", nil))
		var report diagnostics.Report
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if diagnosticCheck(t, report, tc.id).Code != tc.code {
			t.Fatal(w.Body.String())
		}
		if !strings.Contains(logs.text(), "operation_id="+report.OperationID+" scope=instance check="+tc.id+" state=failed code="+tc.code) {
			t.Fatal("diagnostic failure not correlated")
		}
		if strings.Contains(w.Body.String()+logs.text(), "private-engine-secret") || strings.Contains(w.Body.String()+logs.text(), "private-image-name") {
			t.Fatal("raw Docker error leaked")
		}
	}
}

func diagnosticCheck(t *testing.T, r diagnostics.Report, id string) diagnostics.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatal("missing check", id)
	return diagnostics.Check{}
}

func TestDiagnosticsPermissionBoundaryAndScopedReport(t *testing.T) {
	s, sess := accessTestServer(t)
	for _, user := range []string{"alice", "bob", "root"} {
		if err := s.store.CreateToken("diag-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	other := sess
	other.ID = "private-bob-space"
	other.User = "bob"
	if err := s.store.Put(other); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(user, method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer diag-"+user)
		handler.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		user, method, path string
		status             int
	}{
		{"alice", "POST", "/api/diagnostics", 403},
		{"alice", "GET", "/api/diagnostics/ws", 403},
		{"alice", "POST", "/api/sessions/private-bob-space/diagnostics", 404},
		{"alice", "GET", "/api/sessions/private-bob-space/diagnostics/ws", 404},
		{"root", "POST", "/api/sessions/s1/diagnostics", 404},
		{"missing", "POST", "/api/sessions/s1/diagnostics", 401},
	} {
		if w := request(tc.user, tc.method, tc.path); w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	if _, err := s.store.SetQuotaEnforced("alice", true); err != nil {
		t.Fatal(err)
	}
	w := request("alice", "POST", "/api/sessions/s1/diagnostics")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var report diagnostics.Report
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Scope != "session" || report.OperationID != w.Header().Get("X-Agentbox-Operation-ID") {
		t.Fatal("scope/correlation mismatch")
	}
	if diagnosticCheck(t, report, "account_access").Code != "account_access_denied" || diagnosticCheck(t, report, "account_configuration").State != diagnostics.NotChecked {
		t.Fatal("revoked account was inspected")
	}
	if diagnosticCheck(t, report, "quota").Code != "quota_exhausted" || diagnosticCheck(t, report, "model").State != diagnostics.NotChecked {
		t.Fatal("admission/model claim incorrect")
	}
	assertDiagnosticWhitelist(t, w.Body.Bytes())
	for _, secret := range []string{"private-bob-space", "bob", "alice", "selected", "test-only-secret", s.cfg.DataDir} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("scope leak", secret)
		}
	}
	// Administrator sees only the explicitly allowlisted instance report.
	w = request("root", "POST", "/api/diagnostics")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertDiagnosticWhitelist(t, w.Body.Bytes())
	if strings.Contains(w.Body.String(), "private-bob-space") || strings.Contains(w.Body.String(), s.cfg.DataDir) {
		t.Fatal("instance report leaked identity/path")
	}
}

func assertDiagnosticWhitelist(t *testing.T, raw []byte) {
	t.Helper()
	var report map[string]json.RawMessage
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"version": true, "scope": true, "checked_at": true, "operation_id": true, "checks": true}
	for key := range report {
		if !allowed[key] {
			t.Fatal("unexpected diagnostic field", key)
		}
	}
	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(report["checks"], &checks); err != nil {
		t.Fatal(err)
	}
	for _, row := range checks {
		for key := range row {
			if key != "id" && key != "state" && key != "code" && key != "message" && key != "hint" {
				t.Fatal("unexpected check field", key)
			}
		}
		var c diagnostics.Check
		v, _ := json.Marshal(row)
		if err := json.Unmarshal(v, &c); err != nil {
			t.Fatal(err)
		}
		texts, ok := diagnostics.Messages[c.Code]
		if !ok || c.Message != texts[0] || c.Hint != texts[1] {
			t.Fatal("non-allowlisted diagnostic text")
		}
	}
}

func TestDiagnosticsWebSocketIsAuthenticatedAndDoesNotStartWork(t *testing.T) {
	s, sess := accessTestServer(t)
	for _, user := range []string{"alice", "root"} {
		if err := s.store.CreateToken("diag-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	s.upgrader.CheckOrigin = sameHostOrigin
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	const id = "0123456789abcdef0123456789abcdef"
	for _, path := range []string{"/api/diagnostics/ws?token=diag-root", "/api/sessions/s1/diagnostics/ws?token=diag-alice"} {
		url := "ws" + strings.TrimPrefix(srv.URL, "http") + path + "&connection_id=" + id
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var raw map[string]any
		if err := conn.ReadJSON(&raw); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if raw["operation_id"] != id || raw["type"] != "diagnostic" || raw["state"] != "passed" || raw["code"] != "websocket_ok" {
			t.Fatal(raw)
		}
		conn.Close()
		_, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{"https://foreign.invalid"}})
		if err == nil || response == nil || response.StatusCode != 403 {
			t.Fatal("cross-origin probe accepted")
		}
		response.Body.Close()
	}
	// No Docker manager is configured. The WS checks must still work without
	// startup, credential seeding, chat-room creation or accounting.
	if len(s.chat.rooms) != 0 || len(s.store.ListUsage(store.UsageFilter{})) != 0 {
		t.Fatal("diagnostic probe started work")
	}
	after, _ := s.store.Get(sess.ID)
	if after.ContainerID != "" {
		t.Fatal("container created")
	}
}
