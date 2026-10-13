package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"

	"agentbox/internal/config"
	"agentbox/internal/delegation"
	"agentbox/internal/store"
)

type delegationOption struct {
	AccountID    string                 `json:"account_id"`
	AccountLabel string                 `json:"account_label"`
	Agent        string                 `json:"agent"`
	Model        string                 `json:"model"`
	ModelLabel   string                 `json:"model_label"`
	Tags         config.ModelDelegation `json:"tags"`
	Priced       bool                   `json:"priced"`
	PriceIn      float64                `json:"price_in,omitempty"`
	PriceOut     float64                `json:"price_out,omitempty"`
}

// handleDelegationOptions lists the worker models the caller may offer to the
// main agent of this workspace. Only Claude Code workspaces can delegate.
func (s *Server) handleDelegationOptions(w http.ResponseWriter, r *http.Request, sess store.Session) {
	u := reqUser(r)
	workers := []delegationOption{}
	for _, acct := range s.cfg.AccountList() {
		if !s.canUseAccount(acct, u.Name) {
			continue
		}
		for _, m := range acct.Models {
			if m.Delegation == nil || !m.Delegation.Enabled {
				continue
			}
			o := delegationOption{AccountID: acct.ID, AccountLabel: acct.Label, Agent: acct.Type, Model: m.ID, ModelLabel: m.Label, Tags: *m.Delegation}
			if p, _, ok := s.cfg.PriceLookup(acct.Type, m.ID); ok {
				o.Priced, o.PriceIn, o.PriceOut = true, p.Input, p.Output
			}
			workers = append(workers, o)
		}
	}
	d := s.cfg.GetDecision()
	jev := d.Plugins.Jev.Enabled && d.Plugins.Jev.APIKey != "" && d.Plugins.Jev.CanUse(u.Name, u.Role == store.RoleAdmin)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     store.DelegationVersion,
		"available":   sess.Agent == config.AgentClaude,
		"workers":     workers,
		"jev":         jev,
		"jev_default": jev && d.DefaultProvider == "jev",
		"task_types":  config.DelegationTaskTypes,
		"risks":       config.DelegationRisks,
		"max_calls":   delegation.MaxCallsPerTurn,
		"max_workers": store.MaxDelegationWorkers,
	})
}

func (s *Server) ownedDelegation(w http.ResponseWriter, r *http.Request, sess store.Session) (store.Delegation, bool) {
	d, err := s.store.Delegation(sess.User, r.PathValue("delegation"))
	if err != nil || d.SessionID != sess.ID {
		if err != nil && !errors.Is(err, store.ErrDelegationNotFound) {
			writeProblem(w, r, "delegation.read", "internal_error")
		} else {
			writeErr(w, http.StatusNotFound, "委派记录不存在")
		}
		return d, false
	}
	return d, true
}

func (s *Server) handleDelegationGet(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d, ok := s.ownedDelegation(w, r, sess)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.delegationView(d))
}

// handleDelegationLog pages through the worker's own transcript by line.
func (s *Server) handleDelegationLog(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d, ok := s.ownedDelegation(w, r, sess)
	if !ok {
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if after < 0 {
		after = 0
	}
	root, err := s.openDataDir(s.sessionDir(sess))
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	entries := []json.RawMessage{}
	next, more := after, false
	f, err := root.OpenFile(filepath.Join("delegations", d.ID+".jsonl"))
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		line, size := 0, 0
		for scanner.Scan() {
			line++
			if line <= after {
				continue
			}
			if len(entries) >= 1000 || size > 4<<20 {
				more = true
				break
			}
			raw := append(json.RawMessage(nil), scanner.Bytes()...)
			if json.Valid(raw) {
				entries = append(entries, raw)
				size += len(raw)
			}
			next = line
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": next, "has_more": more, "state": d.State})
}

func (s *Server) handleDelegationStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.DelegationStats()
	if err != nil {
		writeProblem(w, r, "delegation.stats", "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
}
