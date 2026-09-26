package pricecatalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func reviewed(t *testing.T) Catalog {
	t.Helper()
	c, err := Parse(bundled, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Version = "fixture-v2"
	for key, e := range c.Entries {
		e.VerifiedAt = "2026-08-11T00:00:00Z"
		c.Entries[key] = e
	}
	return c
}

func TestCatalogStrictValidation(t *testing.T) {
	c := reviewed(t)
	raw, _ := json.Marshal(c)
	if _, err := Parse(raw, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(bundled, true); err == nil {
		t.Fatal("legacy snapshot passed as reviewed")
	}
	for _, bad := range []string{string(raw) + " {}", strings.Replace(string(raw), `"input":10,`, "", 1), strings.Replace(string(raw), `"input":10`, `"input":-1`, 1), strings.Replace(string(raw), `"schema":1`, `"schema":2`, 1), strings.Replace(string(raw), `"source_url":"https:`, `"source_url":"javascript:`, 1), strings.Replace(string(raw), `"input":10`, `"unknown":10`, 1)} {
		if _, err := Parse([]byte(bad), true); err == nil {
			t.Fatal("accepted malformed catalog")
		}
	}
}

func TestCatalogCheckCacheFailureAndSourceChange(t *testing.T) {
	c := reviewed(t)
	requests := 0
	fail := false
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if fail {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(c)
	}))
	defer remote.Close()
	s := New(t.TempDir())
	s.client = remote.Client()
	got := s.Check(context.Background(), remote.URL, true)
	if got.Bundled || got.Error != "" || got.Catalog.Version != c.Version {
		t.Fatal(got)
	}
	s.Check(context.Background(), remote.URL, true)
	if requests != 1 {
		t.Fatal("manual cooldown ignored")
	}
	s.status.AttemptedAt = time.Now().Add(-2 * time.Minute).UnixMilli()
	s.Check(context.Background(), remote.URL, false)
	if requests != 1 {
		t.Fatal("daily cooldown ignored")
	}
	fail = true
	got = s.Check(context.Background(), remote.URL, true)
	if got.Error == "" || got.Revision != c.Revision() || got.CheckedAt == 0 {
		t.Fatal("lost good catalog after failure")
	}
	reloaded := New(s.cacheDir).Snapshot(remote.URL)
	if reloaded.Revision != c.Revision() || reloaded.Bundled {
		t.Fatal("cache not restored")
	}
	changed := s.Snapshot("https://different.invalid/catalog.json")
	if !changed.Bundled || changed.CheckedAt != 0 {
		t.Fatal("reused another source's candidate")
	}
}

func TestCatalogRejectsOversizeAndCancels(t *testing.T) {
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat(" ", MaxBytes+1))) }))
	defer remote.Close()
	s := New(t.TempDir())
	s.client = remote.Client()
	if _, err := s.fetch(context.Background(), remote.URL); err == nil {
		t.Fatal("oversize accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.fetch(ctx, remote.URL); err == nil {
		t.Fatal("ignored cancellation")
	}
	if err := New(t.TempDir()).client.CheckRedirect(httptest.NewRequest("GET", "http://example.invalid", nil), nil); err == nil {
		t.Fatal("redirect downgraded HTTPS")
	}
}
