package server

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"reflect"
	"sort"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/pricecatalog"
)

type priceChange struct {
	Model           string              `json:"model"`
	Kind            string              `json:"kind"` // new | update | custom | current | removed
	Current         *config.ModelPrice  `json:"current,omitempty"`
	Candidate       *pricecatalog.Entry `json:"candidate,omitempty"`
	AutoBlockReason string              `json:"auto_block_reason,omitempty"`
}

type pricingView struct {
	Active            config.PricingState `json:"active"`
	Candidate         pricecatalog.Status `json:"candidate"`
	Changes           []priceChange       `json:"changes"`
	Warnings          []pricingWarning    `json:"warnings"`
	WarningsTruncated bool                `json:"warnings_truncated"`
	WarningError      string              `json:"warning_error,omitempty"`
}

type pricingWarning struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
	Kind  string `json:"kind"`
	Key   string `json:"key"`
}

func (s *Server) priceCatalog() *pricecatalog.Service {
	s.priceCatalogOnce.Do(func() {
		dir := s.cfg.CacheDir
		if dir == "" {
			dir = s.cfg.DataDir
		}
		s.prices = pricecatalog.New(dir)
	})
	return s.prices
}

func pricingChanges(active config.PricingState, candidate pricecatalog.Catalog) []priceChange {
	out := []priceChange{}
	for key, entry := range candidate.Entries {
		change := priceChange{Model: key, Kind: "new", Candidate: &entry}
		if price, ok := active.Prices[key]; ok {
			change.Current = &price
			if _, follows := active.Managed[key]; !follows {
				change.Kind = "custom"
			} else if reflect.DeepEqual(price, entry.Price) {
				change.Kind = "current"
			} else {
				change.Kind = "update"
			}
			if change.Kind == "update" || change.Kind == "current" {
				change.AutoBlockReason = automaticPriceBlock(active, key, entry.Price)
			}
		}
		out = append(out, change)
	}
	for key := range active.Managed {
		if _, ok := candidate.Entries[key]; !ok {
			price := active.Prices[key]
			out = append(out, priceChange{Model: key, Kind: "removed", Current: &price})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (s *Server) pricingView() pricingView {
	active := s.cfg.PricingState()
	candidate := s.priceCatalog().Snapshot(active.Catalog.URL)
	v := pricingView{Active: active, Candidate: candidate, Changes: pricingChanges(active, candidate.Catalog), Warnings: []pricingWarning{}}
	seen := map[string]bool{}
	add := func(agent, model string) {
		id := agent + "/" + model
		if seen[id] {
			return
		}
		seen[id] = true
		_, key, ok := config.LookupPrice(active.Prices, agent, model)
		if ok && key != agent {
			return
		}
		kind := "unpriced"
		if ok {
			kind = "fallback"
		}
		v.Warnings = append(v.Warnings, pricingWarning{Agent: agent, Model: model, Kind: kind, Key: key})
	}
	for agent, model := range s.cfg.GetDefaultModels() {
		add(agent, model)
	}
	for _, sess := range s.store.All() {
		add(sess.Agent, sess.DefaultModel)
	}
	used, truncated, err := s.store.RecentUsageModels(time.Now().AddDate(0, 0, -30), 200)
	if err != nil {
		v.WarningError = "读取最近使用模型失败"
	}
	v.WarningsTruncated = truncated
	for _, model := range used {
		add(model.Agent, model.Model)
	}
	sort.Slice(v.Warnings, func(i, j int) bool {
		return v.Warnings[i].Agent+v.Warnings[i].Model < v.Warnings[j].Agent+v.Warnings[j].Model
	})
	return v
}

func (s *Server) writePricing(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.pricingView())
}
func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) { s.writePricing(w) }
func (s *Server) handlePricingCheck(w http.ResponseWriter, r *http.Request) {
	s.priceCatalog().Check(r.Context(), s.cfg.GetPricingCatalog().URL, true)
	s.writePricing(w)
}

func decodePricing(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, pricecatalog.MaxBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return false
	}
	return true
}
func pricingError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, config.ErrPricingConflict) {
		status = http.StatusConflict
	}
	writeErr(w, status, err.Error())
}

func (s *Server) handlePricingSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision     string                       `json:"revision"`
		Prices       map[string]config.ModelPrice `json:"prices"`
		Catalog      *config.PricingCatalogConfig `json:"catalog"`
		CustomModels []string                     `json:"custom_models"`
	}
	if !decodePricing(w, r, &req) {
		return
	}
	if err := s.cfg.UpdatePricing(req.Revision, req.Prices, nil, req.Catalog, "手动编辑", req.CustomModels...); err != nil {
		pricingError(w, err)
		return
	}
	s.writePricing(w)
}

func (s *Server) handlePricingApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision        string   `json:"revision"`
		CatalogRevision string   `json:"catalog_revision"`
		Models          []string `json:"models"`
		AdoptCustom     []string `json:"adopt_custom"`
	}
	if !decodePricing(w, r, &req) {
		return
	}
	active := s.cfg.PricingState()
	candidate := s.priceCatalog().Snapshot(active.Catalog.URL)
	if req.Revision != active.Revision || req.CatalogRevision != candidate.Revision {
		pricingError(w, config.ErrPricingConflict)
		return
	}
	if len(req.Models) == 0 || len(req.Models) > 200 {
		writeErr(w, 400, "请选择需要应用的模型")
		return
	}
	adopt := map[string]bool{}
	for _, k := range req.AdoptCustom {
		adopt[k] = true
	}
	for _, key := range req.Models {
		entry, ok := candidate.Catalog.Entries[key]
		if !ok {
			writeErr(w, 400, "候选目录中不存在模型 "+key)
			return
		}
		_, exists := active.Prices[key]
		_, follows := active.Managed[key]
		if exists && !follows && !adopt[key] {
			writeErr(w, 400, "自定义价格受保护，请明确选择恢复跟随："+key)
			return
		}
		active.Prices[key] = entry.Price
		active.Managed[key] = catalogOrigin(candidate, entry)
	}
	if err := s.cfg.UpdatePricing(req.Revision, active.Prices, active.Managed, nil, "应用目录 "+candidate.Catalog.Version); err != nil {
		pricingError(w, err)
		return
	}
	s.writePricing(w)
}

func (s *Server) handlePricingRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision string `json:"revision"`
		ID       string `json:"id"`
	}
	if !decodePricing(w, r, &req) {
		return
	}
	if err := s.cfg.RestorePricing(req.Revision, req.ID); err != nil {
		pricingError(w, err)
		return
	}
	s.writePricing(w)
}

func (s *Server) pricingLoop() {
	for {
		source := s.cfg.GetPricingCatalog()
		if source.AutoCheck {
			before := s.priceCatalog().Snapshot(source.URL)
			candidate := s.priceCatalog().Check(s.workContext(), source.URL, false)
			// Apply only a newly fetched successful catalog, never a cached
			// candidate after a rollback, failed fetch, or switch of source.
			if candidate.CheckedAt > before.CheckedAt {
				if err := s.applyAutomaticPricing(candidate); err != nil {
					log.Printf("pricing: 自动跟随失败: %v", err)
				}
			}
		}
		if !waitInterval(s.workContext(), time.Minute) {
			return
		}
	}
}

func catalogOrigin(candidate pricecatalog.Status, entry pricecatalog.Entry) config.PriceOrigin {
	return config.PriceOrigin{Version: candidate.Catalog.Version, SourceURL: entry.SourceURL, VerifiedAt: entry.VerifiedAt, CatalogURL: candidate.URL}
}

// Automatic updates are deliberately narrower than manual adoption. A model
// must already follow this exact source; additions and old unbound origins
// require an explicit choice. The guard applies to every bucket and tier.
func automaticPriceBlock(active config.PricingState, key string, next config.ModelPrice) string {
	origin, follows := active.Managed[key]
	old, exists := active.Prices[key]
	if !exists || !follows {
		return "尚未选择跟随此模型"
	}
	if origin.CatalogURL == "" || origin.CatalogURL != active.Catalog.URL {
		return "需手动应用一次，确认跟随当前来源"
	}
	if old.LongContextOver != next.LongContextOver || (old.Long == nil) != (next.Long == nil) {
		return "长上下文规则变化，需手动核对"
	}
	acceptable := func(a, b config.TokenRates) bool {
		before := []float64{a.Input, a.Output, a.CacheRead, a.CacheWrite}
		after := []float64{b.Input, b.Output, b.CacheRead, b.CacheWrite}
		for i, value := range before {
			if value == after[i] {
				continue
			}
			if value <= 0 || after[i] <= 0 || math.Abs(after[i]/value-1) > .25+1e-9 {
				return false
			}
		}
		return true
	}
	if !acceptable(old.TokenRates, next.TokenRates) || (old.Long != nil && !acceptable(*old.Long, *next.Long)) {
		return "单价变化超过 25% 或涉及零价格，需手动核对"
	}
	return ""
}

func (s *Server) applyAutomaticPricing(candidate pricecatalog.Status) error {
	active := s.cfg.PricingState()
	if !active.Catalog.AutoCheck || !active.Catalog.AutoApply || candidate.Bundled || candidate.Error != "" || candidate.CheckedAt == 0 || candidate.URL != active.Catalog.URL {
		return nil
	}
	changed := false
	for key, entry := range candidate.Catalog.Entries {
		if automaticPriceBlock(active, key, entry.Price) != "" || reflect.DeepEqual(active.Prices[key], entry.Price) {
			continue
		}
		active.Prices[key] = entry.Price
		active.Managed[key] = catalogOrigin(candidate, entry)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.cfg.UpdatePricing(active.Revision, active.Prices, active.Managed, nil, "自动跟随目录 "+candidate.Catalog.Version)
}
