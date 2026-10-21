package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// maxKeepaliveMinTokens bounds the threshold a workspace may set; no context
// window is larger.
const maxKeepaliveMinTokens = 2_000_000

type keepaliveLimits struct {
	MaxHours         int   `json:"max_hours"`
	MinContextTokens int64 `json:"min_context_tokens"`
}

// keepaliveView is a workspace's keepalive for its owner: the setting, what
// it comes to, the chat's latest request (for the composer's indicator) and
// the plans running.
type keepaliveView struct {
	Available bool `json:"available"`
	// Reason is why the workspace cannot keep caches: disabled, agent, account.
	Reason   string `json:"reason,omitempty"`
	Enabled  bool   `json:"enabled"`
	Terminal bool   `json:"terminal"`
	// The workspace's own limits; 0 follows Defaults.
	MaxHours         int                 `json:"max_hours"`
	MinContextTokens int64               `json:"min_context_tokens"`
	Defaults         keepaliveLimits     `json:"defaults"`
	Effective        keepaliveLimits     `json:"effective"`
	MaxHoursLimit    int                 `json:"max_hours_limit"`
	IntervalMinutes  int                 `json:"interval_minutes"`
	Chat             *keepaliveChatView  `json:"chat,omitempty"`
	Plans            []keepalivePlanView `json:"plans"`
}

// keepaliveChatView is the chat's latest request within the cache's hour.
type keepaliveChatView struct {
	PromptTokens int64  `json:"prompt_tokens"`
	TTL          string `json:"ttl"`
	At           int64  `json:"at"` // Unix ms
	// What one keepalive would cost, and writing the cache again.
	KeepMicroUSD    int64 `json:"keep_micro_usd"`
	RewriteMicroUSD int64 `json:"rewrite_micro_usd"`
}

type keepalivePlanView struct {
	ID           string `json:"id"`
	Source       string `json:"source"` // chat | terminal
	Thread       string `json:"thread,omitempty"`
	Title        string `json:"title,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Model        string `json:"model"`
	PromptTokens int64  `json:"prompt_tokens"`
	NextAt       int64  `json:"next_at"` // Unix ms
	Until        int64  `json:"until"`
	Runs         int    `json:"runs"`
	MaxRuns      int    `json:"max_runs"`
	LastRead     int64  `json:"last_read,omitempty"`
	KeepMicroUSD int64  `json:"keep_micro_usd"`
}

// keepaliveCost prices reading (or writing) a prompt of the model's cache.
func (s *Server) keepaliveCost(model string, read, write int64) int64 {
	return s.usageService().Price(store.UsageEvent{Agent: config.AgentClaude, Model: model, CacheReadTokens: read, CacheWriteTokens: write}).CostMicroUSD
}

func (s *Server) keepaliveView(sess store.Session) keepaliveView {
	cfg := s.keepaliveConfig(sess)
	v := keepaliveView{Available: cfg.reason == "", Reason: cfg.reason, Enabled: cfg.Enabled, Terminal: cfg.Terminal,
		MaxHours: cfg.MaxHours, MinContextTokens: cfg.MinContextTokens,
		Defaults:        keepaliveLimits{cfg.admin.MaxHours, cfg.admin.MinContextTokens},
		Effective:       keepaliveLimits{cfg.maxHours, cfg.minTokens},
		MaxHoursLimit:   cfg.admin.MaxHoursLimit,
		IntervalMinutes: cfg.admin.IntervalMinutes,
		Plans:           []keepalivePlanView{}}
	if pt, ok := s.keepalive.point(chatSlot(sess.ID)); ok && time.Since(pt.lastUse()) < cacheLifetime {
		prompt := pt.point.Prompt.Prompt
		v.Chat = &keepaliveChatView{PromptTokens: prompt, TTL: pt.point.Prompt.TTL, At: pt.at.UnixMilli(),
			KeepMicroUSD: s.keepaliveCost(pt.point.Model, prompt, 0), RewriteMicroUSD: s.keepaliveCost(pt.point.Model, 0, prompt)}
	}
	interval := time.Duration(cfg.admin.IntervalMinutes) * time.Minute
	for key, p := range s.keepalive.workspacePlans(sess.ID) {
		pv := keepalivePlanView{ID: key.slot, Source: p.source, Thread: p.thread, Cwd: p.cwd, Model: p.point.Model,
			PromptTokens: p.point.Prompt.Prompt, NextAt: p.next.UnixMilli(), Until: p.until.UnixMilli(), Runs: p.runs,
			LastRead: p.last.Read, KeepMicroUSD: s.keepaliveCost(p.point.Model, p.point.Prompt.Prompt, 0)}
		pv.MaxRuns = p.runs
		for at := p.next; at.Before(p.until); at = at.Add(interval) {
			pv.MaxRuns++
		}
		if p.thread != "" {
			room := s.chat.room(sess.ID)
			room.fileMu.Lock()
			if m, ok := s.scanThread(sess, p.thread); ok {
				pv.Title = m.Title
			}
			room.fileMu.Unlock()
		}
		v.Plans = append(v.Plans, pv)
	}
	sort.Slice(v.Plans, func(i, j int) bool {
		if (v.Plans[i].Source == keepaliveChat) != (v.Plans[j].Source == keepaliveChat) {
			return v.Plans[i].Source == keepaliveChat
		}
		return v.Plans[i].ID < v.Plans[j].ID
	})
	return v
}

func (s *Server) handleGetKeepalive(w http.ResponseWriter, r *http.Request, sess store.Session) {
	writeJSON(w, http.StatusOK, s.keepaliveView(sess))
}

func (s *Server) handlePutKeepalive(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var body struct {
		Enabled          bool  `json:"enabled"`
		Terminal         bool  `json:"terminal"`
		MaxHours         int   `json:"max_hours"`
		MinContextTokens int64 `json:"min_context_tokens"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	cfg := s.keepaliveConfig(sess)
	switch {
	case sess.Agent != config.AgentClaude:
		writeErr(w, http.StatusBadRequest, "只有 Claude 空间支持缓存保活")
		return
	case body.Enabled && cfg.admin.Disabled:
		writeErr(w, http.StatusConflict, "管理员已关闭缓存保活")
		return
	case body.MaxHours < 0 || body.MaxHours > cfg.admin.MaxHoursLimit:
		writeErr(w, http.StatusBadRequest, "最长保活时长超出管理员允许的范围")
		return
	case body.MinContextTokens < 0 || body.MinContextTokens > maxKeepaliveMinTokens:
		writeErr(w, http.StatusBadRequest, "上下文门槛无效")
		return
	}
	err := s.store.SetKeepalive(sess.ID, store.WorkspaceKeepalive{Enabled: body.Enabled, Terminal: body.Terminal,
		MaxHours: body.MaxHours, MinContextTokens: body.MinContextTokens})
	if errors.Is(err, store.ErrKeepaliveWorkspace) {
		writeProblem(w, r, "session.access", "session_not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	s.reconcileKeepalive(sess)
	writeJSON(w, http.StatusOK, s.keepaliveView(sess))
}

// handleStopKeepalive ends one plan; the conversation is kept again only
// after its next request.
func (s *Server) handleStopKeepalive(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil ||
		(body.ID != keepaliveChat && !strings.HasPrefix(body.ID, keepaliveTerminal+":")) {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	s.keepalive.forget(keepaliveKey{sess.ID, body.ID})
	writeJSON(w, http.StatusOK, s.keepaliveView(sess))
}
