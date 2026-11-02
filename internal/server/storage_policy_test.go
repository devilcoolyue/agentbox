package server

import (
	"agentbox/internal/config"
	"agentbox/internal/storagepolicy"
	"agentbox/internal/store"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func fixStorageFree(t *testing.T, available *uint64) {
	t.Helper()
	read := readStorageDisks
	t.Cleanup(func() { readStorageDisks = read })
	readStorageDisks = func(*Server) []storageDisk {
		return []storageDisk{{Roles: []string{"data"}, Total: 50 << 30, Available: *available}}
	}
}

func TestStorageAdmissionTiers(t *testing.T) {
	var free uint64
	fixStorageFree(t, &free)
	s := &Server{cfg: &config.Config{DataDir: t.TempDir(), MaxUploadMB: 512, StoragePolicy: config.StoragePolicy{Mount: "/fixture"}}}
	s.cfg.Resources.MinFreeBytes = 3 << 30
	const stop, start = "POST /api/sessions/{id}/stop", "POST /api/sessions/{id}/start"
	const send, create = "PUT /api/sessions/{id}/chat/requests/{request}", "POST /api/sessions"
	for _, tc := range []struct {
		name    string
		free    uint64
		pattern string
		body    int64
		allowed bool
	}{
		{"growth above the reserve", 5 << 30, create, 0, true},
		{"growth below the reserve", 2 << 30, create, 0, false},
		{"wake below the reserve", 2 << 30, start, 0, true},
		{"send below the reserve", 2 << 30, send, 1024, true},
		{"routine route with a large body keeps the reserve", 2 << 30, send, 16 << 20, false},
		{"wake below the floor", 500 << 20, start, 0, false},
		{"send below the floor", 500 << 20, send, 1024, false},
		{"stop below the floor", 500 << 20, stop, 0, true},
		{"stop with a full disk", 0, stop, 0, true},
		{"logout with a full disk", 0, "POST /api/logout", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			free = tc.free
			method, _, _ := strings.Cut(tc.pattern, " ")
			r := httptest.NewRequest(method, "/fixture", nil)
			r.Pattern, r.ContentLength = tc.pattern, tc.body
			release, ok := s.reserveStorageWrite(r)
			if ok != tc.allowed {
				t.Fatalf("allowed = %t", ok)
			}
			if ok {
				release()
			}
			if s.storageReserved != 0 {
				t.Fatal("reservation leaked")
			}
		})
	}
}

// A renamed route would silently fall back to the full reserve, or lose its exemption.
func TestStorageRouteTablesMatchRegisteredRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	mux := s.routes()
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, table := range []map[string]bool{storageExemptRoutes, storageRoutineRoutes} {
		for pattern := range table {
			method, path, _ := strings.Cut(pattern, " ")
			_, got := mux.Handler(httptest.NewRequest(method, param.ReplaceAllString(path, "x"), nil))
			if got != pattern {
				t.Errorf("%s resolves to %q", pattern, got)
			}
		}
	}
}

func storageRouteServer(t *testing.T, free *uint64) (*Server, http.Handler) {
	t.Helper()
	fixStorageFree(t, free)
	ensure := storageEnsure
	t.Cleanup(func() { storageEnsure = ensure })
	storageEnsure = func(context.Context, config.StoragePolicy, string, string, string, string) (storagepolicy.UserUsage, error) {
		return storagepolicy.UserUsage{}, nil
	}
	s, _ := newTestServer(t)
	s.cfg.StoragePolicy.Mount = "/fixture"
	s.cfg.Resources.MinFreeBytes = 3 << 30
	for _, role := range []string{store.RoleAdmin, store.RoleUser} {
		if err := s.store.CreateUser(store.User{Name: role, Role: role, PassHash: "unused"}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.CreateToken(role+"-token", role); err != nil {
			t.Fatal(err)
		}
	}
	return s, s.Handler()
}

func TestStorageRefusalIsALoggedProblem(t *testing.T) {
	free := uint64(2 << 30)
	_, handler := storageRouteServer(t, &free)
	var logs bytes.Buffer
	output := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(output) })
	do := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer user-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	refused := do(http.MethodPost, "/api/sessions")
	if refused.Code != http.StatusInsufficientStorage {
		t.Fatal(refused.Code, refused.Body.String())
	}
	p := assertProblem(t, refused.Body.Bytes(), "storage_full")
	for _, want := range []string{"operation_id=" + p.OperationID, `route="POST /api/sessions"`, "disk=data"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %s: %s", want, logs.String())
		}
	}
	// The middleware sees the matched pattern, so these tiers apply on the real mux.
	free = 500 << 20
	if w := do(http.MethodPost, "/api/sessions/missing/stop"); w.Code == http.StatusInsufficientStorage {
		t.Fatal("stop refused on a full disk")
	}
	free = 2 << 30
	if w := do(http.MethodPost, "/api/sessions/missing/start"); w.Code == http.StatusInsufficientStorage {
		t.Fatal("waking a workspace held to the full reserve")
	}
}

func TestLowStorageIsReportedToAdminsOnly(t *testing.T) {
	free := uint64(5 << 30)
	_, handler := storageRouteServer(t, &free)
	me := func(token string) map[string]json.RawMessage {
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(w.Code, err)
		}
		return body
	}
	if _, ok := me("admin-token")["storage_low"]; !ok {
		t.Fatal("admin not warned below the low-water mark")
	}
	if _, ok := me("user-token")["storage_low"]; ok {
		t.Fatal("disk levels shown to an ordinary user")
	}
	free = 7 << 30
	if _, ok := me("admin-token")["storage_low"]; ok {
		t.Fatal("warned with room to spare")
	}
}

func TestStorageWaterLogsCrossingsOnce(t *testing.T) {
	free := uint64(5 << 30)
	fixStorageFree(t, &free)
	s := &Server{cfg: &config.Config{DataDir: t.TempDir()}}
	s.cfg.Resources.MinFreeBytes = 3 << 30
	var logs bytes.Buffer
	output := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(output) })
	seen := map[string]bool{}
	s.logStorageWater(seen)
	s.logStorageWater(seen)
	if n := strings.Count(logs.String(), "disk running low disk=data"); n != 1 {
		t.Fatalf("low logged %d times: %s", n, logs.String())
	}
	free = 7 << 30
	s.logStorageWater(seen)
	s.logStorageWater(seen)
	if n := strings.Count(logs.String(), "disk recovered disk=data"); n != 1 {
		t.Fatalf("recovery logged %d times: %s", n, logs.String())
	}
}

func TestStorageReservationUsesRequestSizeAndReleases(t *testing.T) {
	s := &Server{cfg: &config.Config{DataDir: t.TempDir(), MaxUploadMB: 1024 * 1024, StoragePolicy: config.StoragePolicy{Mount: "/fixture"}}}
	r := httptest.NewRequest("POST", "/upload", strings.NewReader("small file"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	release, ok := s.reserveStorageWrite(r)
	if !ok {
		t.Fatal("small upload charged configured maximum")
	}
	if s.storageReserved != 1<<20 {
		t.Fatal(s.storageReserved)
	}
	release()
	if s.storageReserved != 0 {
		t.Fatal("reservation leaked")
	}
	s.cfg.Resources.MinFreeBytes = 1 << 62
	if _, ok := s.reserveStorageWrite(r); ok {
		t.Fatal("ignored free-space reserve")
	}
	r.Method = "DELETE"
	if release, ok := s.reserveStorageWrite(r); !ok {
		t.Fatal("blocked cleanup on full disk")
	} else {
		release()
	}
}
