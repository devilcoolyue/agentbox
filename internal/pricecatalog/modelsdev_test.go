package pricecatalog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Synthetic provider data: never use production credentials or billing rows.
const devFixture = `{
 "anthropic":{"models":{"claude-opus-4-6":{"id":"claude-opus-4-6","modalities":{"output":["text"]},"limit":{"context":1000000},"cost":{"input":4,"output":20,"cache_read":0.4,"cache_write":5}}}},
 "openai":{"models":{"gpt-5.5":{"id":"gpt-5.5","modalities":{"output":["text"]},"limit":{"context":1000000},"cost":{"input":2,"output":10,"cache_read":0.2,"tiers":[{"input":4,"output":15,"cache_read":0.4,"tier":{"type":"context","size":272000}}],"context_over_200k":{"input":4,"output":15,"cache_read":0.4}}}}},
 "relay":{"models":{"claude-opus-4-6":{"cost":{"input":0}}}}
}`

func TestModelsDevConvertsCacheTTLAndExplicitContextTier(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	c, err := ParseModelsDev([]byte(devFixture), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 2 || len(c.Issues) != 0 {
		t.Fatal(c)
	}
	claude := c.Entries["claude-opus-4-6"]
	if claude.Price.Input != 4 || claude.Price.CacheWrite != 8 || claude.VerifiedAt != "" || claude.SourceURL != ModelsDevURL {
		t.Fatal(claude)
	}
	gpt := c.Entries["gpt-5.5"].Price
	if gpt.LongContextOver != 272000 || gpt.Long.Output != 15 || gpt.CacheWrite != 0 {
		t.Fatal(gpt)
	}
	later, err := ParseModelsDev([]byte(devFixture), now.Add(time.Hour))
	if err != nil || later.Version != c.Version {
		t.Fatal("version changed merely on retrieval", err)
	}
	// Third-party imports must not masquerade as reviewed custom catalogs.
	encoded, _ := json.Marshal(c)
	if _, err := Parse(encoded, true); err == nil {
		t.Fatal("third-party feed passed human verification requirement")
	}
}

func TestModelsDevIncompleteAndUnrepresentableModelsAreNotApplied(t *testing.T) {
	for _, tc := range []struct{ name, old, next, model string }{
		{"missing input", `"input":4,"output":20`, `"output":20`, "claude-opus-4-6"},
		{"missing cache", `"cache_read":0.4,"cache_write":5`, `"cache_write":5`, "claude-opus-4-6"},
		{"null cache", `"cache_read":0.4,"cache_write":5`, `"cache_read":null,"cache_write":5`, "claude-opus-4-6"},
		{"negative", `"cache_write":5`, `"cache_write":-5`, "claude-opus-4-6"},
		{"changed TTL", `"cache_write":5`, `"cache_write":8`, "claude-opus-4-6"},
		{"Claude tier counting", `"cache_write":5`, `"cache_write":5,"tiers":[{"input":8,"output":30,"cache_read":0.8,"cache_write":10,"tier":{"type":"context","size":200000}}]`, "claude-opus-4-6"},
		{"unknown dimension", `"cache_write":5`, `"cache_write":5,"request":1`, "claude-opus-4-6"},
		{"old long Claude", `claude-opus-4-6`, `claude-sonnet-4-5`, "claude-sonnet-4-5"},
		{"tier type", `"type":"context"`, `"type":"tokens"`, "gpt-5.5"},
		{"missing threshold", `"size":272000`, `"unknown":272000`, "gpt-5.5"},
		{"mismatched legacy tier", `"context_over_200k":{"input":4`, `"context_over_200k":{"input":8`, "gpt-5.5"},
		{"unknown cache-write default", `gpt-5.5`, `gpt-9`, "gpt-9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseModelsDev([]byte(strings.ReplaceAll(devFixture, tc.old, tc.next)), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := c.Entries[tc.model]; ok {
				t.Fatal("invalid price imported", tc.model)
			}
			if len(c.Issues) != 1 || c.Issues[0].Model != tc.model || c.Issues[0].Reason == "" {
				t.Fatal(c.Issues)
			}
		})
	}
	for _, raw := range []string{`{}`, devFixture + `{}`, strings.Repeat(" ", ModelsDevMaxBytes+1)} {
		if _, err := ParseModelsDev([]byte(raw), time.Now()); err == nil {
			t.Fatal("invalid feed accepted")
		}
	}
}

type pricingTransport func(*http.Request) (*http.Response, error)

func (f pricingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelsDevFetchCacheRestartAndFailedUpdate(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	requests := 0
	fail := false
	s.client = &http.Client{Transport: pricingTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != ModelsDevURL {
			t.Fatal(r.URL)
		}
		requests++
		body := devFixture
		if fail {
			body = `{"openai":{}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	first := s.Check(context.Background(), ModelsDevURL, true)
	if first.Error != "" || first.Bundled || first.Catalog.Source != "models.dev" {
		t.Fatal(first)
	}
	reloaded := New(dir).Snapshot(ModelsDevURL)
	if !reflect.DeepEqual(first, reloaded) {
		t.Fatal("lost third-party provenance or prices on restart")
	}
	s.Check(context.Background(), ModelsDevURL, false)
	if requests != 1 {
		t.Fatal("daily cooldown ignored")
	}
	s.status.AttemptedAt = time.Now().Add(-25 * time.Hour).UnixMilli()
	fail = true
	failed := s.Check(context.Background(), ModelsDevURL, false)
	if failed.Error == "" || failed.Revision != first.Revision || failed.CheckedAt != first.CheckedAt {
		t.Fatal("failed fetch replaced good catalog")
	}
	if switched := s.Snapshot("https://example.invalid/catalog.json"); !switched.Bundled || switched.Catalog.Source == "models.dev" {
		t.Fatal("source switch reused third-party candidate")
	}
}
