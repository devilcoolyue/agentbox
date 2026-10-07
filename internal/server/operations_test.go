package server

import (
	"agentbox/internal/config"
	"agentbox/internal/store"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperationsRequireAdminAndDiagnosticsAreAllowlisted(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.AuthToken = "synthetic-secret-marker"
	s.cfg.Accounts = []config.Account{{ID: "private-account", Env: map[string]string{"API_KEY": "private-key"}}}
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		if err := s.store.CreateUser(store.User{Name: role, Role: role, PassHash: "unused"}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.CreateToken(role+"-token", role); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	for _, path := range []string{"/api/storage", "/api/diagnostics", "/api/cache/marketplace"} {
		method := http.MethodGet
		if strings.Contains(path, "cache") {
			method = http.MethodDelete
		}
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer user-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{s.cfg.AuthToken, "private-account", "private-key", s.cfg.DataDir} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("diagnostic leaked private data")
		}
	}
	var data map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, key := range []string{"version", "revision", "built_at", "go_version", "os", "arch", "schema_version", "started_at", "sessions", "disk_total", "disk_available", "resources", "usage_sync", "environment"} {
		allowed[key] = true
	}
	for key := range data {
		if !allowed[key] {
			t.Fatalf("unexpected diagnostic field %q", key)
		}
	}
}
func TestUsageQueryOnlyQueuesBackgroundScan(t *testing.T) {
	s, _ := newTestServer(t)
	getUsageEvents(t, s, "/api/usage/events", "alice", store.RoleUser)
	if s.usageService().SyncStatus().LastScanAt != 0 {
		t.Fatal("HTTP triggered synchronous filesystem scan")
	}
}
