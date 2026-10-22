package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/internal/usage"
	"agentbox/internal/workspace"
)

// Prompt cache keepalive.
//
// A web chat turn is a fresh `claude -p --resume` process, and the provider
// keeps a conversation's prompt cache for an hour after its last use on a
// subscription (the CLI writes five-minute caches for API keys and relays).
// A user who comes back later pays to write the whole conversation into the
// cache again, about twenty times what reading it costs. While the user is
// away, the server resumes the conversation for one throwaway request that
// reads the cache and so restarts its hour (agent.KeepaliveCommand); the CLI
// saves nothing of it. The same request keeps the cache of a Claude Code
// session in the workspace's terminal: a resumed session repeats the system
// prompt and tools its transcript recorded, so it reads what the interactive
// CLI cached.
//
// Each workspace turns it on itself (store.WorkspaceKeepalive), terminal
// sessions separately; the administrator may forbid it and sets the defaults,
// the longest a workspace may ask for and the interval (cache_keepalive).
// Conversations come from two places: a chat turn's end (noteChatCache) and
// the terminal usage scan, which reports each interactive session's latest
// request (noteTerminalCache). The latest point of every conversation is kept
// for an hour whether or not the workspace keeps caches, so turning it on
// starts at once. One plan per conversation slot: the workspace's chat, and
// each terminal working directory (a new session there replaces the old).
//
// Only a conversation whose last request wrote a one-hour cache of at least
// min_context_tokens is kept, for at most max_hours after that request. The
// next request replaces the plan; a running turn, a switched account, a chat
// thread that no longer continues the conversation, a terminal CLI that left
// its session, an exhausted balance or any sign the cache is gone ends it. A
// workspace the idle reaper stopped is woken for a keepalive and put back to
// sleep without telling extensions, and a workspace the user stopped stays
// stopped. The terminal's CLI stops with the container, but the user resumes
// its session on return (`claude -c`, `--resume`), and that new process sends
// the prefix the transcript recorded, so the cache is worth keeping for it
// too (measured against 2.1.304). Keepalive requests are charged like titles
// (kind keepalive). The points survive a restart (keepalive_store.go), and a
// turn the server's own shutdown interrupts leaves its point like one the
// user stopped.

const (
	keepaliveTimeout = 3 * time.Minute
	// cacheLifetime is how long the provider keeps a one-hour cache after
	// its last use.
	cacheLifetime = time.Hour
	// keepaliveTerminalSlots bounds a workspace's terminal plans.
	keepaliveTerminalSlots = 8
)

const (
	keepaliveChat     = "chat"
	keepaliveTerminal = "terminal"
)

// keepaliveKey names a conversation slot: a workspace's chat, or one of its
// terminal working directories.
type keepaliveKey struct{ workspace, slot string }

func chatSlot(id string) keepaliveKey { return keepaliveKey{id, keepaliveChat} }
func terminalSlot(id, cwd string) keepaliveKey {
	return keepaliveKey{id, keepaliveTerminal + ":" + cwd}
}

// String names the slot in logs: the workspace and "chat" or "terminal <dir>".
func (key keepaliveKey) String() string {
	if key.slot == keepaliveChat {
		return key.workspace + " chat"
	}
	return key.workspace + " terminal " + strings.TrimPrefix(key.slot, keepaliveTerminal+":")
}

// keepalivePoint is the latest request of a conversation.
type keepalivePoint struct {
	source  string // keepaliveChat | keepaliveTerminal
	thread  string // chat: the thread the conversation continues
	cwd     string // terminal: where the CLI ran
	account string
	point   chat.CachePoint
	at      time.Time // the request
	warm    time.Time // the latest keepalive that read the cache
}

// lastUse is when the provider last used the cache, which starts its hour.
func (p keepalivePoint) lastUse() time.Time {
	if p.warm.After(p.at) {
		return p.warm
	}
	return p.at
}

type keepalivePlan struct {
	keepalivePoint
	gen   uint64
	until time.Time // no keepalive starts later
	next  time.Time
	runs  int
	last  usage.PromptCache // what the latest keepalive read
	timer *time.Timer
}

type keepaliveState struct {
	mu     sync.Mutex
	gen    uint64
	points map[keepaliveKey]*keepalivePoint
	plans  map[keepaliveKey]*keepalivePlan
	// path is keepalive-points.json; dirty wakes keepaliveSaver.
	path   string
	dirty  chan struct{}
	saveMu sync.Mutex
}

// note keeps p as the slot's latest point unless the slot knows a later one
// or this very request (a terminal rescan reports old sessions too, and the
// one after a restart reports again what keepalive-points.json brought back).
func (k *keepaliveState) note(key keepaliveKey, p keepalivePoint) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.points == nil {
		k.points = map[keepaliveKey]*keepalivePoint{}
	}
	now := time.Now()
	for other, q := range k.points {
		if now.Sub(q.lastUse()) >= cacheLifetime && k.plans[other] == nil {
			delete(k.points, other)
		}
	}
	if old := k.points[key]; old != nil && (old.at.After(p.at) || old.at.Equal(p.at) && old.point.Conversation == p.point.Conversation) {
		return false
	}
	k.points[key] = &p
	if p.source == keepaliveTerminal {
		k.trimTerminalLocked(key.workspace)
	}
	k.changedLocked()
	return true
}

// trimTerminalLocked drops the workspace's least recent terminal slots beyond
// keepaliveTerminalSlots.
func (k *keepaliveState) trimTerminalLocked(id string) {
	var keys []keepaliveKey
	for key, p := range k.points {
		if key.workspace == id && p.source == keepaliveTerminal {
			keys = append(keys, key)
		}
	}
	for len(keys) > keepaliveTerminalSlots {
		oldest := 0
		for i, key := range keys {
			if k.points[key].at.Before(k.points[keys[oldest]].at) {
				oldest = i
			}
		}
		k.forgetLocked(keys[oldest])
		keys = append(keys[:oldest], keys[oldest+1:]...)
	}
}

func (k *keepaliveState) forget(key keepaliveKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.forgetLocked(key)
}

// drop forgets the slot, saying why when it had a plan.
func (k *keepaliveState) drop(key keepaliveKey, why string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.dropLocked(key, why)
	delete(k.points, key)
}

func (k *keepaliveState) forgetLocked(key keepaliveKey) {
	k.cancelLocked(key)
	delete(k.points, key)
}

// dropLocked cancels the slot's plan, saying why when there was one.
func (k *keepaliveState) dropLocked(key keepaliveKey, why string) {
	if k.plans[key] != nil {
		log.Printf("cache keepalive %s: plan dropped: %s", key, why)
	}
	k.cancelLocked(key)
}

// forgetConversation drops the slot if it still holds conversation.
func (k *keepaliveState) forgetConversation(key keepaliveKey, conversation, why string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if p := k.points[key]; p != nil && p.point.Conversation == conversation {
		k.dropLocked(key, why)
		delete(k.points, key)
	}
}

func (k *keepaliveState) slots(id string) []keepaliveKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []keepaliveKey
	for key := range k.points {
		if key.workspace == id {
			out = append(out, key)
		}
	}
	return out
}

func (k *keepaliveState) point(key keepaliveKey) (keepalivePoint, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p := k.points[key]
	if p == nil {
		return keepalivePoint{}, false
	}
	return *p, true
}

// workspacePlans returns copies of the workspace's plans.
func (k *keepaliveState) workspacePlans(id string) map[keepaliveKey]keepalivePlan {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[keepaliveKey]keepalivePlan{}
	for key, p := range k.plans {
		if key.workspace == id {
			out[key] = *p
		}
	}
	return out
}

func (k *keepaliveState) armLocked(s *Server, key keepaliveKey, p *keepalivePlan) {
	k.cancelLocked(key)
	if k.plans == nil {
		k.plans = map[keepaliveKey]*keepalivePlan{}
	}
	k.gen++
	p.gen = k.gen
	gen := p.gen
	p.timer = time.AfterFunc(time.Until(p.next), func() { s.spawn(func() { s.runKeepalive(key, gen) }) })
	k.plans[key] = p
}

func (k *keepaliveState) current(key keepaliveKey, gen uint64) (keepalivePlan, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p := k.plans[key]
	if p == nil || p.gen != gen {
		return keepalivePlan{}, false
	}
	return *p, true
}

// cancelLocked ends the slot's plan. Every change to points and plans comes
// through here or note, which is where they are saved.
func (k *keepaliveState) cancelLocked(key keepaliveKey) {
	if p := k.plans[key]; p != nil {
		p.timer.Stop()
		delete(k.plans, key)
	}
	k.changedLocked()
}

// keepaliveConfig is what a workspace's keepalive comes to: its own setting
// within the administrator's.
type keepaliveConfig struct {
	store.WorkspaceKeepalive
	admin     config.CacheKeepaliveConfig
	maxHours  int
	minTokens int64
	interval  time.Duration
	// reason says why the workspace cannot keep caches at all: "disabled"
	// by the administrator, an "agent" without them, or an "account" (API
	// key or relay) that writes five-minute caches. "" when it can.
	reason string
}

func (s *Server) keepaliveConfig(sess store.Session) keepaliveConfig {
	c := keepaliveConfig{admin: s.cfg.GetCacheKeepalive()}
	if own, err := s.store.Keepalive(sess.ID); err != nil {
		log.Printf("cache keepalive %s: %v", sess.ID, err)
	} else {
		c.WorkspaceKeepalive = own
	}
	c.maxHours, c.minTokens = c.admin.MaxHours, c.admin.MinContextTokens
	if c.MaxHours > 0 {
		c.maxHours = min(c.MaxHours, c.admin.MaxHoursLimit)
	}
	if c.MinContextTokens > 0 {
		c.minTokens = c.MinContextTokens
	}
	c.interval = time.Duration(c.admin.IntervalMinutes) * time.Minute
	switch acct, _ := s.cfg.Account(sess.AccountID); {
	case c.admin.Disabled:
		c.reason = "disabled"
	case sess.Agent != config.AgentClaude:
		c.reason = "agent"
	case shortCacheAccount(acct):
		c.reason = "account"
	}
	return c
}

// shortCacheAccount reports a Claude API key or relay account, for which the
// CLI writes five-minute caches unless the account asks for an hour.
func shortCacheAccount(acct config.Account) bool {
	_, token := claudeRelay(acct)
	return token != "" && acct.Env["CLAUDE_CODE_PROMPT_CACHE_TTL"] != "1h"
}

// keeps reports whether conversations of source are kept at all.
func (c keepaliveConfig) keeps(source string) bool {
	return c.reason == "" && c.Enabled && (source == keepaliveChat || c.Terminal)
}

// worth reports whether a conversation's last request is worth keeping.
func (c keepaliveConfig) worth(p usage.PromptCache) bool {
	return p.TTL == "1h" && p.Prompt >= c.minTokens
}

// why says what keeps the point from being kept; "" when it is kept.
func (c keepaliveConfig) why(pt keepalivePoint, account string) string {
	switch {
	case c.reason != "":
		return "unavailable (" + c.reason + ")"
	case !c.Enabled:
		return "turned off for the workspace"
	case pt.source == keepaliveTerminal && !c.Terminal:
		return "terminal sessions are not kept"
	case pt.point.Prompt.TTL != "1h":
		return fmt.Sprintf("the last request wrote no one-hour cache (%q)", pt.point.Prompt.TTL)
	case pt.point.Prompt.Prompt < c.minTokens:
		return fmt.Sprintf("the last prompt had %d tokens, below %d", pt.point.Prompt.Prompt, c.minTokens)
	case pt.account != account:
		return "the workspace switched accounts"
	}
	return ""
}

// noteChatCache takes the conversation a web chat turn just ended in. It must
// not block.
func (s *Server) noteChatCache(o chat.TurnOutcome) {
	key := chatSlot(o.SessionID)
	// Close interrupts running turns, which then fail; what one sent is cached
	// all the same, and the user resumes the conversation after the restart.
	failed := o.Status == chat.OutcomeFailed && !s.stopping()
	// /compact replaced the conversation the cache held with a summary; the
	// next request writes the cache anew and plans from there.
	if o.Cache != nil && o.Cache.Prompt.Compacted && !failed {
		s.keepalive.drop(key, "the conversation was compacted")
		return
	}
	// A turn that sent no request (a local command such as /context) left the
	// cache's hour where it was, and the plan with it.
	if o.Cache != nil && o.Cache.Prompt.Prompt == 0 && !failed {
		return
	}
	sess, ok := s.store.Get(o.SessionID)
	if !ok || o.Cache == nil || failed || o.ThreadID == "" {
		s.keepalive.forget(key)
		return
	}
	s.keepalive.note(key, keepalivePoint{source: keepaliveChat, thread: o.ThreadID, account: sess.AccountID,
		point: *o.Cache, at: time.Now()})
	s.planKeepalive(sess, key)
}

// noteTerminalCache takes the latest request of an interactive Claude session
// in a workspace terminal, from the usage scan. It must not block.
func (s *Server) noteTerminalCache(sess store.Session, c usage.TerminalCache) {
	if sess.Agent != config.AgentClaude || c.Cwd == "" || c.Conversation == "" {
		return
	}
	key := terminalSlot(sess.ID, c.Cwd)
	if c.Exited {
		s.keepalive.forgetConversation(key, c.Conversation, "the CLI left session "+c.Conversation)
		return
	}
	if c.Prompt.Compacted {
		s.keepalive.forgetConversation(key, c.Conversation, "session "+c.Conversation+" was compacted")
		return
	}
	if time.Since(c.At) >= cacheLifetime {
		return // a rescan of an old session; its cache is gone
	}
	point := chat.CachePoint{Conversation: c.Conversation, Model: c.Model, Prompt: c.Prompt}
	// The recorded effort is repeated when the CLI takes it as a flag; the
	// resumed session's recorded prompt matches without it as well.
	if slices.Contains(config.ReasoningLevels(config.AgentClaude, "effort"), c.Effort) {
		point.Effort, point.Control = c.Effort, "effort"
	}
	if s.keepalive.note(key, keepalivePoint{source: keepaliveTerminal, cwd: c.Cwd, account: sess.AccountID, point: point, at: c.At}) {
		s.planKeepalive(sess, key)
	}
}

// planKeepalive (re)plans the slot's keepalive from its latest point, or
// drops the plan when the workspace does not keep it.
func (s *Server) planKeepalive(sess store.Session, key keepaliveKey) {
	cfg := s.keepaliveConfig(sess)
	pause, paused := s.quotaPause(sess)
	k := &s.keepalive
	k.mu.Lock()
	defer k.mu.Unlock()
	pt := k.points[key]
	if pt == nil {
		k.dropLocked(key, "no request to keep")
		return
	}
	if why := cfg.why(*pt, sess.AccountID); why != "" {
		k.dropLocked(key, why)
		return
	}
	now := time.Now()
	if now.Sub(pt.lastUse()) >= cacheLifetime {
		k.dropLocked(key, "the cache is older than an hour")
		delete(k.points, key)
		return
	}
	until := pt.at.Add(time.Duration(cfg.maxHours) * time.Hour)
	if paused {
		// A five-hour pause keeps the cache for the turn that resumes after
		// the reset, within the longest a workspace may ask for.
		reset := time.UnixMilli(pause.ResetsAt).Add(quotaKeepPastReset)
		if limit := pt.at.Add(time.Duration(cfg.admin.MaxHoursLimit) * time.Hour); reset.After(limit) {
			reset = limit
		}
		if reset.After(until) {
			until = reset
		}
	}
	next := pt.lastUse().Add(cfg.interval)
	if next.Before(now) {
		next = now.Add(time.Second)
	}
	if !next.Before(until) {
		k.dropLocked(key, "past the longest keepalive")
		return
	}
	plan := &keepalivePlan{keepalivePoint: *pt, until: until, next: next}
	old := k.plans[key]
	if old != nil && old.keepalivePoint == *pt {
		if old.until.Equal(until) && old.next.Equal(next) {
			return
		}
		plan.runs, plan.last = old.runs, old.last
	}
	if old == nil {
		// Each request of a busy conversation moves the plan; only its start is told.
		log.Printf("cache keepalive %s: planned, first read at %s for a %d-token prompt (%s)",
			key, next.UTC().Format(time.TimeOnly), pt.point.Prompt.Prompt, pt.point.Conversation)
	}
	k.armLocked(s, key, plan)
}

// reconcileKeepalive replans every conversation of the workspace after its
// setting or the administrator's changed.
func (s *Server) reconcileKeepalive(sess store.Session) {
	for _, key := range s.keepalive.slots(sess.ID) {
		s.planKeepalive(sess, key)
	}
}

// reconcileAllKeepalive replans every workspace's conversations.
func (s *Server) reconcileAllKeepalive() {
	seen := map[string]bool{}
	s.keepalive.mu.Lock()
	for key := range s.keepalive.points {
		seen[key.workspace] = true
	}
	s.keepalive.mu.Unlock()
	for id := range seen {
		if sess, ok := s.store.Get(id); ok {
			s.reconcileKeepalive(sess)
		} else {
			for _, key := range s.keepalive.slots(id) {
				s.keepalive.forget(key)
			}
		}
	}
}

func (s *Server) runKeepalive(key keepaliveKey, gen uint64) {
	plan, ok := s.keepalive.current(key, gen)
	if !ok {
		return
	}
	cache, again, err := s.keepCache(key.workspace, plan)
	if err != nil {
		log.Printf("cache keepalive %s: %v", key, err)
	}
	k := &s.keepalive
	k.mu.Lock()
	defer k.mu.Unlock()
	p := k.plans[key]
	if p == nil || p.gen != gen {
		return // replaced meanwhile
	}
	if cache.Prompt > 0 {
		p.runs++
		p.last = cache
	}
	if !again {
		k.forgetLocked(key)
		return
	}
	now := time.Now()
	if pt := k.points[key]; pt != nil && pt.at.Equal(p.at) {
		pt.warm = now
	}
	p.warm = now
	p.next = now.Add(time.Duration(s.cfg.GetCacheKeepalive().IntervalMinutes) * time.Minute)
	if !p.next.Before(p.until) {
		k.cancelLocked(key)
		return
	}
	k.armLocked(s, key, p)
}

var (
	errKeepaliveCold = errors.New("the prompt cache was already gone; keepalive stopped")
	// errKeepaliveStopped wraps every reason a plan ends without a request.
	errKeepaliveStopped = errors.New("keepalive stopped")
)

func keepaliveStopped(why string) (usage.PromptCache, bool, error) {
	return usage.PromptCache{}, false, fmt.Errorf("%w: %s", errKeepaliveStopped, why)
}

// keepCache sends one keepalive request and reports what it read and whether
// the cache is still worth keeping. A plan that ends without a request says
// why in an errKeepaliveStopped error.
func (s *Server) keepCache(id string, plan keepalivePlan) (usage.PromptCache, bool, error) {
	sess, ok := s.store.Get(id)
	if !ok || sess.Agent != config.AgentClaude {
		return keepaliveStopped("the workspace is gone")
	}
	if why := s.keepaliveConfig(sess).why(plan.keepalivePoint, sess.AccountID); why != "" {
		return keepaliveStopped(why)
	}
	if block := s.quotaBlock(sess.User); block != "" {
		return keepaliveStopped("the balance is used up")
	}
	if plan.source == keepaliveChat {
		// The thread must still continue this conversation, and no turn may
		// be running: it would refresh the cache itself and plan anew when it
		// ends.
		room := s.chat.room(id)
		room.fileMu.Lock()
		resume := s.threadResumeID(sess, plan.thread)
		room.fileMu.Unlock()
		if resume != plan.point.Conversation {
			return keepaliveStopped("the thread no longer continues the conversation")
		}
		if s.chatTurnRunning(id) {
			return keepaliveStopped("a turn is running")
		}
	}
	ctx, cancel := context.WithTimeout(s.workContext(), keepaliveTimeout)
	defer cancel()
	woken, woke, err := s.workspaces().Wake(ctx, id)
	if errors.Is(err, workspace.ErrNotAsleep) {
		return keepaliveStopped("the workspace was stopped by the user")
	}
	if err != nil {
		return usage.PromptCache{}, false, err
	}
	sess = woken
	if woke {
		since := time.Now()
		defer func() {
			sleepCtx, cancel := context.WithTimeout(s.workContext(), 30*time.Second)
			defer cancel()
			if err := s.workspaces().Sleep(sleepCtx, id, since); err != nil {
				log.Printf("cache keepalive %s: back to sleep: %v", id, err)
			}
		}()
	}
	p := plan.point
	cmd, err := agent.KeepaliveCommand(p.Conversation, plan.cwd, p.Model, p.Effort, p.Control, p.TaskTools)
	if err != nil {
		return usage.PromptCache{}, false, err
	}
	base := store.UsageEvent{User: sess.User, SessionID: sess.ID, ThreadID: plan.thread, TurnID: store.NewID(),
		Agent: sess.Agent, AccountID: sess.AccountID, Model: p.Model, Kind: store.UsageKindKeepalive}
	tally := s.usageService().NewChatTally(sess)
	var lines int
	var result keepaliveResult
	executor := chat.Executor{Backend: s.dock, Log: log.Printf,
		Environment: func() ([]string, error) { return s.execEnv(sess) },
		OnLine: func(line []byte) {
			if len(line) > 0 {
				lines++
				result.observe(line)
				tally.Observe(base, line)
				if w, ok := agent.FiveHourWindow(line); ok {
					s.noteFiveHour(sess, w)
				}
			}
		}}
	started := time.Now()
	runErr := executor.RunCommand(ctx, chat.Turn{SessionID: sess.ID, ContainerID: sess.ContainerID, Text: agent.KeepalivePrompt}, cmd)
	// Whatever the exit status, a request that went out has been paid for.
	s.usageService().Flush(&tally, time.Since(started))
	cache := tally.PromptCache()
	switch {
	case cache.Prompt == 0:
		// Nothing was sent: say what the CLI answered instead.
		return cache, false, errors.Join(fmt.Errorf("%w: the CLI sent no request (%d output lines; %s)", errKeepaliveStopped, lines, result), runErr)
	case cache.TTL == "5m" || cache.Read*2 < cache.Prompt:
		return cache, false, errors.Join(errKeepaliveCold, runErr)
	}
	key := chatSlot(id)
	if plan.source == keepaliveTerminal {
		key = terminalSlot(id, plan.cwd)
	}
	log.Printf("cache keepalive %s: read %d of %d prompt tokens from cache", key, cache.Read, cache.Prompt)
	return cache, true, runErr
}

// keepaliveResult keeps the CLI's final result of a keepalive for the log: its
// subtype and the start of its text, which for a keepalive is "OK" or the
// CLI's own error, never the conversation.
type keepaliveResult struct {
	seen    bool
	subtype string
	isError bool
	text    string
}

func (r *keepaliveResult) observe(line []byte) {
	if !bytes.HasPrefix(line, []byte(`{"type":"result"`)) {
		return
	}
	var ev struct {
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	r.seen, r.subtype, r.isError, r.text = true, ev.Subtype, ev.IsError, truncRunes(ev.Result, 200)
}

func (r keepaliveResult) String() string {
	if !r.seen {
		return "no result"
	}
	return fmt.Sprintf("result %s, is_error %v: %q", r.subtype, r.isError, r.text)
}

// chatTurnRunning reports whether the workspace's chat room runs a turn.
func (s *Server) chatTurnRunning(id string) bool {
	if s.chat == nil {
		return false
	}
	s.chat.mu.Lock()
	room := s.chat.rooms[id]
	s.chat.mu.Unlock()
	if room == nil {
		return false
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	return room.running
}
