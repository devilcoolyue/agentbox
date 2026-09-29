package config

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestPricingConcurrentEditsOnlyOneApplies(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	revision := c.PricingState().Revision
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, price := range []float64{1, 2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- c.UpdatePricing(revision, map[string]ModelPrice{"fixture": {TokenRates: TokenRates{Input: price}}}, nil, nil, "edit")
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrPricingConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	s := c.PricingState()
	if err := c.UpdatePricing(s.Revision, s.Prices, nil, nil, "no change"); err != nil {
		t.Fatal(err)
	}
	if len(c.PricingState().History) != len(s.History) {
		t.Fatal("no-op created history")
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PricingState().Revision != s.Revision {
		t.Fatal("empty managed map changed revision after restart")
	}
}

func TestPricingFollowManualEditRestoreAndPersistence(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	old := c.PricingState()
	prices := c.GetPricing()
	prices["fixture"] = ModelPrice{TokenRates: TokenRates{Input: 2}, LongContextOver: 100, Long: &TokenRates{Input: 4}}
	origins := map[string]PriceOrigin{"fixture": {Version: "v1", SourceURL: "https://example.invalid/pricing", VerifiedAt: "2026-08-11T00:00:00Z"}}
	source := &PricingCatalogConfig{URL: "https://example.invalid/catalog.json", AutoCheck: true}
	if err := c.UpdatePricing(old.Revision, prices, origins, source, "测试导入"); err != nil {
		t.Fatal(err)
	}
	imported := c.PricingState()
	if err := c.UpdatePricing(old.Revision, prices, nil, nil, "过期编辑"); !errors.Is(err, ErrPricingConflict) {
		t.Fatalf("stale write: %v", err)
	}
	mode := "acceptEdits"
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(imported, reloaded.PricingState()) {
		t.Fatal("unrelated settings write lost pricing metadata/history")
	}
	prices["fixture"] = ModelPrice{TokenRates: TokenRates{Input: 9}}
	if err := c.ApplySettings(SettingsPatch{Pricing: prices}); err != nil {
		t.Fatal(err)
	}
	edited := c.PricingState()
	if _, ok := edited.Managed["fixture"]; ok {
		t.Fatal("manual edit still follows catalog")
	}
	if err := c.RestorePricing(edited.Revision, edited.History[0].ID); err != nil {
		t.Fatal(err)
	}
	restored := c.PricingState()
	if restored.Prices["fixture"].Input != 2 || restored.Managed["fixture"].Version != "v1" || restored.Catalog != *source {
		t.Fatal(restored)
	}
	// Returned plan and history must not provide mutable access to live state.
	restored.Prices["fixture"].Long.Input = 99
	restored.History[1].Prices["fixture"] = ModelPrice{}
	if c.GetPricing()["fixture"].Long.Input != 4 {
		t.Fatal("returned mutable tier")
	}
}

func TestPricingFailedSaveAndInvalidSourceDoNotMutate(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	before := c.PricingState()
	for _, source := range []PricingCatalogConfig{{URL: "https://models.dev/api.json", AutoApply: true}, {AutoCheck: true}, {URL: "http://example.invalid"}, {URL: "https://user:secret@example.invalid"}} {
		if err := c.UpdatePricing(before.Revision, nil, nil, &source, ""); err == nil {
			t.Fatal("accepted bad source")
		}
	}
	c.path = t.TempDir() // rename onto a directory fails
	if err := c.UpdatePricing(before.Revision, map[string]ModelPrice{}, nil, nil, "delete"); err == nil {
		t.Fatal("expected write failure")
	}
	if !reflect.DeepEqual(before, c.PricingState()) {
		t.Fatal("failed write mutated prices/history")
	}
}

func TestPricingCustomWithoutRateChangeAndHistoryBound(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	for i := 0; i < 15; i++ {
		s := c.PricingState()
		prices := s.Prices
		prices["fixture"] = ModelPrice{TokenRates: TokenRates{Input: float64(i)}}
		if err := c.UpdatePricing(s.Revision, prices, map[string]PriceOrigin{"fixture": {Version: "v1"}}, nil, "update"); err != nil {
			t.Fatal(err)
		}
	}
	s := c.PricingState()
	if len(s.History) != 10 {
		t.Fatal(len(s.History))
	}
	if err := c.UpdatePricing(s.Revision, s.Prices, nil, nil, "custom", "fixture"); err != nil {
		t.Fatal(err)
	}
	if len(c.PricingState().Managed) != 0 {
		t.Fatal("explicit custom ignored")
	}
}
