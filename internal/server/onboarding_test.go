package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
)

func TestOnboardingIsActorScopedAndDoesNotClaimModelAvailability(t *testing.T) {
	s, _ := accessTestServer(t)
	limits := s.cfg.GetContainer()
	limits.CPUs, limits.MemoryMB, limits.PidsLimit = 1.5, 768, 96
	limits.Network = "private-network-name"
	if err := s.cfg.ApplySettings(config.SettingsPatch{Container: &limits}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ANTHROPIC_API_KEY": "synthetic-private-api-key"}
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Env: &env}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob", "root"} {
		if err := s.store.CreateToken("setup-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	for _, tc := range []struct {
		user             string
		count            int
		admin, hasSpaces bool
	}{{"alice", 1, false, true}, {"bob", 2, false, false}, {"root", 3, true, false}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/onboarding?user=root", nil)
		r.Header.Set("Authorization", "Bearer setup-"+tc.user)
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		var result onboardingView
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Version != 1 || result.CanConfigure != tc.admin || result.HasWorkspaces != tc.hasSpaces || !result.CanCreate || len(result.Accounts) != tc.count {
			t.Fatalf("wrong scoped snapshot: %s", w.Body.String())
		}
		if !result.Accounts[0].CredentialsPresent {
			t.Fatal("API-key configuration missing")
		}
		if !tc.admin && len(result.DefaultModels) != 1 {
			t.Fatal("unavailable agent defaults exposed")
		}
		if tc.admin && len(result.DefaultModels) != 2 {
			t.Fatal("administrator cannot prepare both defaults")
		}
		if tc.user == "alice" && (strings.Contains(w.Body.String(), "selected") || strings.Contains(w.Body.String(), "admins")) {
			t.Fatal("unauthorized accounts disclosed")
		}
		for _, secret := range []string{"synthetic-private-api-key", "test-only-secret", s.cfg.DataDir, "model_available", "access_token", "private-network-name"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("private data or unverified availability in snapshot")
			}
		}
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &keys)
		if len(keys) != 7 {
			t.Fatal("unexpected onboarding fields", w.Body.String())
		}
		var resources map[string]any
		_ = json.Unmarshal(keys["container_resources"], &resources)
		if len(resources) != 3 || resources["cpus"] != 1.5 || resources["memory_mb"] != float64(768) || resources["pids_limit"] != float64(96) {
			t.Fatal("resource projection omitted limits or exposed private configuration", resources)
		}
		var accounts []map[string]any
		_ = json.Unmarshal(keys["accounts"], &accounts)
		for _, account := range accounts {
			if len(account) != 5 || account["default_model"] != s.cfg.GetDefaultModel(account["type"].(string)) {
				t.Fatal("account projection gained private fields or lost the new-workspace model", account)
			}
		}
	}
	if err := s.cfg.ApplySettings(config.SettingsPatch{DefaultModels: map[string]string{config.AgentClaude: "claude-sonnet-5"}}); err != nil {
		t.Fatal(err)
	}
	changed := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/api/onboarding", nil)
	request.Header.Set("Authorization", "Bearer setup-alice")
	handler.ServeHTTP(changed, request)
	var updated onboardingView
	if err := json.Unmarshal(changed.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.DefaultModels[config.AgentClaude] != "claude-sonnet-5" {
		t.Fatal("guide returned stale defaults")
	}
	// An account with its own model list starts new workspaces on its default.
	relayDefault := "relay-b"
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Models: &[]config.ModelOption{{ID: "relay-a"}, {ID: "relay-b"}}, DefaultModel: &relayDefault}); err != nil {
		t.Fatal(err)
	}
	listed := httptest.NewRecorder()
	request = httptest.NewRequest("GET", "/api/onboarding", nil)
	request.Header.Set("Authorization", "Bearer setup-alice")
	handler.ServeHTTP(listed, request)
	var own onboardingView
	if err := json.Unmarshal(listed.Body.Bytes(), &own); err != nil || len(own.Accounts) != 1 || own.Accounts[0].DefaultModel != "relay-b" {
		t.Fatal("account model list not used for new workspaces", listed.Body.String())
	}
	if err := s.cfg.RemoveAccount("shared"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/onboarding", nil)
	r.Header.Set("Authorization", "Bearer setup-alice")
	handler.ServeHTTP(w, r)
	var result onboardingView
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.CanCreate || len(result.Accounts) != 0 || len(result.DefaultModels) != 0 {
		t.Fatal("revoked/no-account state still offers creation")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/onboarding", nil))
	if w.Code != 401 {
		t.Fatal("public setup snapshot")
	}
}

func TestCreateWorkspaceStorageErrorIsSafe(t *testing.T) {
	s, _ := accessTestServer(t)
	s.cfg.DataDir = filepath.Join(t.TempDir(), "private-missing-directory")
	w := httptest.NewRecorder()
	s.handleCreateSession(w, accessRequest("alice", "POST", "/sessions", `{"name":"first project","agent":"claude","account_id":"shared"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatal(w.Code, w.Body.String())
	}
	assertProblem(t, w.Body.Bytes(), "internal_error")
	if strings.Contains(w.Body.String(), "private-missing-directory") {
		t.Fatal("creation leaked host path")
	}
	if len(s.store.All()) != 1 {
		t.Fatal("failed creation added session")
	}
}
