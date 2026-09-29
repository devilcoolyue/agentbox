package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"time"
)

// Checks discover candidates; automatic application requires an explicit opt-in.
type PricingCatalogConfig struct {
	URL       string `json:"url"`
	AutoCheck bool   `json:"auto_check"`
	AutoApply bool   `json:"auto_apply,omitempty"`
}

type PriceOrigin struct {
	Version    string `json:"version"`
	SourceURL  string `json:"source_url"`
	VerifiedAt string `json:"verified_at,omitempty"`
	CatalogURL string `json:"catalog_url,omitempty"`
}

type PricingRevision struct {
	ID      string                 `json:"id"`
	SavedAt int64                  `json:"saved_at"`
	Reason  string                 `json:"reason"`
	Prices  map[string]ModelPrice  `json:"prices"`
	Managed map[string]PriceOrigin `json:"managed"`
}

type PricingState struct {
	Revision string                 `json:"revision"`
	Prices   map[string]ModelPrice  `json:"prices"`
	Managed  map[string]PriceOrigin `json:"managed"`
	Catalog  PricingCatalogConfig   `json:"catalog"`
	History  []PricingRevision      `json:"history"`
}

var ErrPricingConflict = errors.New("价目表已变化，请刷新后重新核对")

func ValidateCatalogURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(raw) > 2048 {
		return fmt.Errorf("价格目录地址必须是无账号密码和片段的 HTTPS URL")
	}
	return nil
}

func ValidatePrices(prices map[string]ModelPrice) error {
	clean, err := sanitizePricing(prices)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(clean, prices) {
		return fmt.Errorf("模型 ID 不得含首尾空白")
	}
	return nil
}

func (c *Config) validatePricing() error {
	if err := ValidateCatalogURL(c.PricingCatalog.URL); err != nil {
		return err
	}
	if c.PricingCatalog.AutoCheck && c.PricingCatalog.URL == "" {
		return fmt.Errorf("自动检查需要配置价格目录地址")
	}
	if c.PricingCatalog.AutoApply && !c.PricingCatalog.AutoCheck {
		return fmt.Errorf("自动跟随需要开启每天自动检查")
	}
	if _, err := sanitizePricing(c.Pricing); err != nil {
		return err
	}
	if len(c.PricingHistory) > 10 {
		return fmt.Errorf("价格历史最多保留 10 个版本")
	}
	return nil
}

func clonePrices(in map[string]ModelPrice) map[string]ModelPrice {
	out := make(map[string]ModelPrice, len(in))
	for k, p := range in {
		if p.Long != nil {
			r := *p.Long
			p.Long = &r
		}
		out[k] = p
	}
	return out
}

func cloneOrigins(in map[string]PriceOrigin) map[string]PriceOrigin {
	out := make(map[string]PriceOrigin, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func retainedOrigins(old, next map[string]ModelPrice, managed map[string]PriceOrigin) map[string]PriceOrigin {
	out := map[string]PriceOrigin{}
	for k, origin := range managed {
		if p, ok := next[k]; ok && reflect.DeepEqual(p, old[k]) {
			out[k] = origin
		}
	}
	return out
}

func pricingRevision(prices map[string]ModelPrice, managed map[string]PriceOrigin, source PricingCatalogConfig) string {
	// omitempty reloads empty maps as nil; that must not change the revision.
	if len(prices) == 0 {
		prices = map[string]ModelPrice{}
	}
	if len(managed) == 0 {
		managed = map[string]PriceOrigin{}
	}
	raw, _ := json.Marshal([]any{prices, managed, source})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func (c *Config) PricingState() PricingState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := PricingState{Prices: clonePrices(c.Pricing), Managed: cloneOrigins(c.PricingManaged), Catalog: c.PricingCatalog, History: []PricingRevision{}}
	out.Revision = pricingRevision(c.Pricing, c.PricingManaged, c.PricingCatalog)
	for _, h := range c.PricingHistory {
		h.Prices = clonePrices(h.Prices)
		h.Managed = cloneOrigins(h.Managed)
		out.History = append(out.History, h)
	}
	return out
}

func (c *Config) GetPricingCatalog() PricingCatalogConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PricingCatalog
}

// PricingPlan is an immutable per-turn copy, without administrative history.
func (c *Config) PricingPlan() PricingState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return PricingState{Prices: clonePrices(c.Pricing), Managed: cloneOrigins(c.PricingManaged), Revision: pricingRevision(c.Pricing, c.PricingManaged, c.PricingCatalog)}
}

// Called only on mutate's working copy. A failed save never changes live history.
func (c *Config) replacePricing(prices map[string]ModelPrice, managed map[string]PriceOrigin, reason string) {
	if pricingRevision(c.Pricing, c.PricingManaged, c.PricingCatalog) == pricingRevision(prices, managed, c.PricingCatalog) {
		return
	}
	old := PricingRevision{ID: pricingRevision(c.Pricing, c.PricingManaged, c.PricingCatalog), SavedAt: time.Now().UnixMilli(), Reason: reason, Prices: clonePrices(c.Pricing), Managed: cloneOrigins(c.PricingManaged)}
	c.PricingHistory = append([]PricingRevision{old}, c.PricingHistory...)
	if len(c.PricingHistory) > 10 {
		c.PricingHistory = c.PricingHistory[:10]
	}
	c.Pricing, c.PricingManaged = clonePrices(prices), cloneOrigins(managed)
}

// UpdatePricing uses one optimistic revision for prices, follow flags and source.
// managed == nil means a manual edit: changed rows become custom automatically.
func (c *Config) UpdatePricing(expected string, prices map[string]ModelPrice, managed map[string]PriceOrigin, source *PricingCatalogConfig, reason string, custom ...string) error {
	return c.mutate(func(w *Config) error {
		if expected == "" || expected != pricingRevision(w.Pricing, w.PricingManaged, w.PricingCatalog) {
			return ErrPricingConflict
		}
		if prices != nil {
			if err := ValidatePrices(prices); err != nil {
				return err
			}
			if managed == nil {
				managed = retainedOrigins(w.Pricing, prices, w.PricingManaged)
			}
			for _, key := range custom {
				delete(managed, key)
			}
			w.replacePricing(prices, managed, reason)
		}
		if source != nil {
			w.PricingCatalog = *source
		}
		return nil
	})
}

func (c *Config) RestorePricing(expected, id string) error {
	return c.mutate(func(w *Config) error {
		if expected == "" || expected != pricingRevision(w.Pricing, w.PricingManaged, w.PricingCatalog) {
			return ErrPricingConflict
		}
		for _, h := range w.PricingHistory {
			if h.ID == id {
				w.replacePricing(h.Prices, h.Managed, "回退价格版本")
				// A rollback must survive the next scheduled check.
				w.PricingCatalog.AutoApply = false
				return nil
			}
		}
		return fmt.Errorf("价格历史版本不存在")
	})
}
