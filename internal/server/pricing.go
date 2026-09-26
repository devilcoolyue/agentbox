package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/pricecatalog"
)

type priceChange struct {
	Model     string              `json:"model"`
	Kind      string              `json:"kind"` // new | update | custom | current | removed
	Current   *config.ModelPrice  `json:"current,omitempty"`
	Candidate *pricecatalog.Entry `json:"candidate,omitempty"`
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
		active.Managed[key] = config.PriceOrigin{Version: candidate.Catalog.Version, SourceURL: entry.SourceURL, VerifiedAt: entry.VerifiedAt}
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
			s.priceCatalog().Check(s.workContext(), source.URL, false)
		}
		if !waitInterval(s.workContext(), time.Minute) {
			return
		}
	}
}
