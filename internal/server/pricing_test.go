package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/pricecatalog"
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

func TestAutomaticPricingProtectsCustomNewSourceAndAnomalies(t *testing.T) {
	s := pricingTestServer(t)
	source := config.PricingCatalogConfig{URL: pricecatalog.ModelsDevURL, AutoCheck: true, AutoApply: true}
	rate := func(n float64) config.ModelPrice {
		return config.ModelPrice{TokenRates: config.TokenRates{Input: n, Output: n * 4, CacheRead: n / 10, CacheWrite: 0}}
	}
	prices := map[string]config.ModelPrice{}
	origins := map[string]config.PriceOrigin{}
	for _, key := range []string{"follow", "custom", "removed", "large", "zero", "tier", "other-source", "legacy-origin"} {
		prices[key] = rate(4)
		if key != "custom" {
			origins[key] = config.PriceOrigin{Version: "old", CatalogURL: source.URL}
		}
	}
	origins["other-source"] = config.PriceOrigin{Version: "old", CatalogURL: "https://example.invalid/prices"}
	origins["legacy-origin"] = config.PriceOrigin{Version: "old"}
	initial := s.cfg.PricingState()
	if err := s.cfg.UpdatePricing(initial.Revision, prices, origins, &source, "fixture"); err != nil {
		t.Fatal(err)
	}
	before := s.cfg.PricingState()
	candidate := pricecatalog.Status{URL: source.URL, CheckedAt: time.Now().UnixMilli(), Catalog: pricecatalog.Catalog{Version: "new", Entries: map[string]pricecatalog.Entry{}}}
	for _, key := range []string{"follow", "custom", "new", "large", "zero", "tier", "other-source", "legacy-origin"} {
		candidate.Catalog.Entries[key] = pricecatalog.Entry{Price: rate(5), SourceURL: source.URL}
	}
	candidate.Catalog.Entries["large"] = pricecatalog.Entry{Price: rate(6)}
	candidate.Catalog.Entries["zero"] = pricecatalog.Entry{Price: rate(0)}
	tier := rate(5)
	tier.LongContextOver = 272000
	tier.Long = &config.TokenRates{Input: 10}
	candidate.Catalog.Entries["tier"] = pricecatalog.Entry{Price: tier}
	for _, bad := range []pricecatalog.Status{{URL: source.URL, Bundled: true}, {URL: source.URL, Error: "failed"}, {URL: "https://example.invalid/catalog"}} {
		bad.Catalog = candidate.Catalog
		bad.CheckedAt = candidate.CheckedAt
		if err := s.applyAutomaticPricing(bad); err != nil {
			t.Fatal(err)
		}
		if s.cfg.PricingState().Revision != before.Revision {
			t.Fatal("bad candidate changed prices")
		}
	}
	if err := s.applyAutomaticPricing(candidate); err != nil {
		t.Fatal(err)
	}
	after := s.cfg.PricingState()
	if after.Prices["follow"].Input != 5 || after.Managed["follow"].Version != "new" {
		t.Fatal("follow not updated")
	}
	for key, price := range before.Prices {
		if key != "follow" && !reflect.DeepEqual(after.Prices[key], price) {
			t.Fatal("protected price changed", key)
		}
	}
	if _, ok := after.Prices["new"]; ok {
		t.Fatal("new model auto-adopted")
	}
	if len(after.History) != len(before.History)+1 {
		t.Fatal("missing history")
	}
	if err := s.applyAutomaticPricing(candidate); err != nil {
		t.Fatal(err)
	}
	if s.cfg.PricingState().Revision != after.Revision {
		t.Fatal("no-op update changed revision")
	}
	if err := s.cfg.RestorePricing(after.Revision, before.Revision); err != nil {
		t.Fatal(err)
	}
	restored := s.cfg.PricingState()
	if restored.Catalog.AutoApply || restored.Prices["follow"].Input != 4 {
		t.Fatal("rollback not protected")
	}
	if err := s.applyAutomaticPricing(candidate); err != nil {
		t.Fatal(err)
	}
	if s.cfg.PricingState().Revision != restored.Revision {
		t.Fatal("rollback overwritten")
	}
	reloaded, err := config.Load(s.cfg.Path())
	if err != nil || !reflect.DeepEqual(reloaded.PricingState(), restored) {
		t.Fatal("lost auto-follow/source binding on restart", err)
	}
}

func TestAutomaticPriceGuardChecksEveryTierAndThreshold(t *testing.T) {
	source := "https://example.invalid/catalog"
	old := config.ModelPrice{TokenRates: config.TokenRates{Input: 4, Output: 20, CacheRead: .4, CacheWrite: 8}, LongContextOver: 272000, Long: &config.TokenRates{Input: 8, Output: 30, CacheRead: .8, CacheWrite: 16}}
	active := config.PricingState{Catalog: config.PricingCatalogConfig{URL: source}, Prices: map[string]config.ModelPrice{"fixture": old}, Managed: map[string]config.PriceOrigin{"fixture": {CatalogURL: source}}}
	for _, tc := range []struct {
		name    string
		change  func(*config.ModelPrice)
		blocked bool
	}{
		{"unchanged", func(p *config.ModelPrice) {}, false},
		{"25 percent increase", func(p *config.ModelPrice) { p.Long.Output = 37.5 }, false},
		{"25 percent decrease", func(p *config.ModelPrice) { p.Output = 15 }, false},
		{"above limit", func(p *config.ModelPrice) { p.Long.CacheRead = 1.001 }, true},
		{"free", func(p *config.ModelPrice) { p.CacheWrite = 0 }, true},
		{"threshold", func(p *config.ModelPrice) { p.LongContextOver = 200000 }, true},
		{"remove tier", func(p *config.ModelPrice) { p.Long = nil; p.LongContextOver = 0 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := old
			long := *old.Long
			p.Long = &long
			tc.change(&p)
			if got := automaticPriceBlock(active, "fixture", p); (got != "") != tc.blocked {
				t.Fatal(got)
			}
		})
	}
}
