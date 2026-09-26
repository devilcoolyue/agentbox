package store

import (
	"encoding/json"
	"math"
)

// PriceSnapshot records the decision used when a row was first priced. Both
// tiers are retained so a growing terminal turn keeps its original price table.
type PriceSnapshot struct {
	PricingRevision string      `json:"pricing_revision,omitempty"`
	CatalogVersion  string      `json:"catalog_version,omitempty"`
	SourceURL       string      `json:"source_url,omitempty"`
	VerifiedAt      string      `json:"verified_at,omitempty"`
	Version         int         `json:"version"`
	Source          string      `json:"source"` // provider | table | unpriced
	Key             string      `json:"key,omitempty"`
	Standard        TokenRates  `json:"standard"`
	Long            *TokenRates `json:"long,omitempty"`
	LongContextOver int64       `json:"long_context_over,omitempty"`
}
type TokenRates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

func (p *PriceSnapshot) Rates(prompt int64) (TokenRates, bool) {
	if p.Long != nil && p.LongContextOver > 0 && prompt > p.LongContextOver {
		return *p.Long, true
	}
	return p.Standard, false
}
func (p *PriceSnapshot) Cost(e UsageEvent) int64 {
	if p.Source == "unpriced" {
		return 0
	}
	r, _ := p.Rates(e.InputTokens + e.CacheReadTokens)
	return int64(math.Round(float64(e.InputTokens)*r.Input + float64(e.OutputTokens)*r.Output + float64(e.CacheReadTokens)*r.CacheRead + float64(e.CacheWriteTokens)*r.CacheWrite))
}
func priceJSON(p *PriceSnapshot) (string, error) {
	if p == nil {
		return "", nil
	}
	raw, err := json.Marshal(p)
	return string(raw), err
}
