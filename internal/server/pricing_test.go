package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func pricingTestServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"fixture-secret","agent_image":"fixture","max_upload_mb":10,"pricing":{"claude-opus-5":{"input":99,"output":99}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg = cfg
	return s
}

func pricingRequest(handler http.HandlerFunc, method string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(method, "/api/pricing", bytes.NewReader(raw)))
	return w
}

func TestPricingPreviewProtectionApplyConflictAndRestore(t *testing.T) {
	s := pricingTestServer(t)
	before := s.cfg.PricingState()
	v := s.pricingView()
	for _, change := range v.Changes {
		if change.Model == "claude-opus-5" && change.Kind != "custom" {
			t.Fatal(change)
		}
	}
	body := map[string]any{"revision": v.Active.Revision, "catalog_revision": v.Candidate.Revision, "models": []string{"claude-opus-5"}}
	if w := pricingRequest(s.handlePricingApply, "POST", body); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.cfg.GetPricing()["claude-opus-5"].Input != 99 {
		t.Fatal("custom price overwritten")
	}
	body["adopt_custom"] = []string{"claude-opus-5"}
	if w := pricingRequest(s.handlePricingApply, "POST", body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.cfg.PricingState().Managed["claude-opus-5"].Version == "" {
		t.Fatal("did not adopt catalog")
	}
	if w := pricingRequest(s.handlePricingApply, "POST", body); w.Code != 409 {
		t.Fatal("stale apply accepted", w.Code)
	}
	now := s.cfg.PricingState()
	if w := pricingRequest(s.handlePricingRestore, "POST", map[string]string{"revision": now.Revision, "id": before.Revision}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.cfg.GetPricing()["claude-opus-5"].Input != 99 || len(s.cfg.PricingState().Managed) != 0 {
		t.Fatal("restore failed")
	}
	// An unavailable remote check is descriptive and does not change live prices.
	revision := s.cfg.PricingState().Revision
	if w := pricingRequest(s.handlePricingCheck, "POST", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if s.cfg.PricingState().Revision != revision {
		t.Fatal("check modified active prices")
	}
}

func TestPricingWarningIncludesFallbackBlankAndRecentModels(t *testing.T) {
	s := pricingTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 2}}}
	for _, e := range []store.UsageEvent{
		{TS: time.Now(), Agent: "codex", Model: "fixture-new", Kind: "chat"},
		{TS: time.Now(), Agent: "codex", Model: "", Kind: "chat"},
		{TS: time.Now(), Agent: "claude", Model: "fixture-unpriced", Kind: "terminal"},
		{TS: time.Now().AddDate(0, 0, -40), Agent: "claude", Model: "fixture-old", Kind: "chat"},
	} {
		if err := s.store.InsertUsage(e); err != nil {
			t.Fatal(err)
		}
	}
	v := s.pricingView()
	found := map[string]string{}
	for _, w := range v.Warnings {
		found[w.Model] = w.Kind
	}
	if found["fixture-new"] != "fallback" || found[""] != "fallback" || found["fixture-unpriced"] != "unpriced" || found["fixture-old"] != "" {
		t.Fatal(found)
	}
}

func TestPricingRoutesRequireAdmin(t *testing.T) {
	s := pricingTestServer(t)
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		if err := s.store.CreateUser(store.User{Name: role, Role: role, PassHash: "unused"}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.CreateToken(role+"-token", role); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	for _, route := range []struct{ method, path string }{{"GET", "/api/pricing"}, {"PUT", "/api/pricing"}, {"POST", "/api/pricing/check"}, {"POST", "/api/pricing/apply"}, {"POST", "/api/pricing/restore"}} {
		for _, role := range []string{"", store.RoleUser} {
			r := httptest.NewRequest(route.method, route.path, nil)
			if role != "" {
				r.Header.Set("Authorization", "Bearer "+role+"-token")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 401
			if role != "" {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("%s %s %d", role, route.path, w.Code)
			}
		}
		if route.method != "GET" {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(route.method, route.path+"?token=admin-token", nil))
			if w.Code != 401 {
				t.Fatal("query token authorized mutation")
			}
		}
	}
}
