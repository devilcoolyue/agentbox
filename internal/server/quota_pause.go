package server

import (
	"log"
	"sync"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/protocol"
	"agentbox/internal/store"
)

// Five-hour pause.
//
// A Claude subscription's usage runs in five-hour windows. A long turn can use
// a window up, and then the cache keepalive's requests are refused too, so the
// cache it keeps goes cold before the window starts over. A workspace keeping
// caches therefore pauses its chat once a model response reports the window
// at the workspace's threshold (its keepalive setting, by default
// cache_keepalive.pause_percent): the running turn stops at its next model
// request (chat.Service), the way the time limit stops it; messages sent
// meanwhile queue (an ordinary send is refused); and a "继续" is queued that
// runs, like any queued follow-up, once the window has started over. The
// keepalive keeps the workspace's caches at least that long, within the
// administrator's limit. Raising or turning off the threshold ends the pause
// at once.
//
// The windows come from the rate_limit_event Claude Code passes on with every
// response (chat turns and keepalives alike) and are kept per account, so a
// pause outlives a restart (store.RateWindow). Usage elsewhere (the terminal,
// another computer) shows once a web request reports it.

// quotaResumeText is the message queued to carry on after the reset.
const quotaResumeText = "继续"

// quotaResumeDelay is how long after the reported reset queued work starts,
// for clock differences.
const quotaResumeDelay = time.Minute

// quotaKeepPastReset is how long past the reset a pause keeps the caches, so
// the resumed turn finds them.
const quotaKeepPastReset = 15 * time.Minute

// errChatPaused refuses an ordinary send while the workspace is paused.
var errChatPaused = chatRequestProblem("chat_quota_reserved")

// quotaWindows is every account's latest five-hour window.
type quotaWindows struct {
	mu      sync.Mutex
	windows map[string]store.RateWindow
	armed   map[string]time.Time // the reset each account's timer waits for
	timers  map[string]*time.Timer
}

func (q *quotaWindows) window(account string) (store.RateWindow, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	w, ok := q.windows[account]
	return w, ok
}

// set records w and reports whether it changed anything.
func (q *quotaWindows) set(account string, w store.RateWindow) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.windows == nil {
		q.windows = map[string]store.RateWindow{}
	}
	if old, ok := q.windows[account]; ok && old.Percent == w.Percent && old.ResetsAt.Equal(w.ResetsAt) {
		return false
	}
	q.windows[account] = w
	return true
}

// quotaPauseView is a paused workspace's state for its pages.
type quotaPauseView struct {
	Percent   float64 `json:"percent"`
	Threshold int     `json:"threshold"`
	ResetsAt  int64   `json:"resets_at"` // Unix ms
}

// pausePercent is the five-hour usage at which the workspace pauses; 0 when
// it does not keep caches or turned the pause off.
func (s *Server) pausePercent(sess store.Session) int {
	c := s.keepaliveConfig(sess)
	switch {
	case c.reason != "" || !c.Enabled || c.PausePercent < 0:
		return 0
	case c.PausePercent > 0:
		return c.PausePercent
	}
	return c.admin.PausePercent
}

// quotaPause reports whether the workspace is paused: its account's latest
// window reached the threshold and has not started over.
func (s *Server) quotaPause(sess store.Session) (quotaPauseView, bool) {
	threshold := s.pausePercent(sess)
	if threshold == 0 || sess.AccountID == "" {
		return quotaPauseView{}, false
	}
	w, ok := s.quota.window(sess.AccountID)
	if !ok || w.Percent < float64(threshold) || !time.Now().Before(w.ResetsAt) {
		return quotaPauseView{}, false
	}
	return quotaPauseView{Percent: w.Percent, Threshold: threshold, ResetsAt: w.ResetsAt.UnixMilli()}, true
}

func (s *Server) quotaPaused(id string) (quotaPauseView, bool) {
	sess, ok := s.store.Get(id)
	if !ok {
		return quotaPauseView{}, false
	}
	return s.quotaPause(sess)
}

// noteFiveHour records the window a response on the workspace's account
// reported and says whether the workspace pauses at it.
func (s *Server) noteFiveHour(sess store.Session, w agent.RateWindow) bool {
	if sess.AccountID == "" || !w.ResetsAt.After(time.Now()) {
		return false
	}
	rw := store.RateWindow{Percent: w.Percent, ResetsAt: w.ResetsAt}
	if s.quota.set(sess.AccountID, rw) {
		if err := s.store.SetRateWindow(sess.AccountID, rw); err != nil {
			log.Printf("five-hour window %s: %v", sess.AccountID, err)
		}
		s.armQuotaReset(sess.AccountID, rw.ResetsAt)
		s.publishQuotaPause(sess.AccountID)
	}
	_, paused := s.quotaPause(sess)
	return paused
}

// loadQuotaWindows brings back the windows that have not started over.
func (s *Server) loadQuotaWindows() {
	windows, err := s.store.RateWindows(time.Now())
	if err != nil {
		log.Printf("five-hour windows: %v", err)
		return
	}
	for account, w := range windows {
		s.quota.set(account, w)
		s.armQuotaReset(account, w.ResetsAt)
	}
}

// armQuotaReset wakes the queues once the account's window starts over.
func (s *Server) armQuotaReset(account string, at time.Time) {
	q := &s.quota
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.armed == nil {
		q.armed, q.timers = map[string]time.Time{}, map[string]*time.Timer{}
	}
	if q.armed[account].Equal(at) {
		return
	}
	if t := q.timers[account]; t != nil {
		t.Stop()
	}
	q.armed[account] = at
	q.timers[account] = time.AfterFunc(time.Until(at)+quotaResumeDelay, func() {
		if !s.stopping() {
			s.publishQuotaPause(account)
			s.dispatchQueuedChats()
		}
	})
}

// publishQuotaPause tells the open pages of the account's workspaces whether
// they are paused.
func (s *Server) publishQuotaPause(account string) {
	if s.chat == nil {
		return
	}
	s.chat.mu.Lock()
	rooms := make([]*chatRoom, 0, len(s.chat.rooms))
	for _, r := range s.chat.rooms {
		rooms = append(rooms, r)
	}
	s.chat.mu.Unlock()
	for _, r := range rooms {
		if sess, ok := s.store.Get(r.sessID); ok && sess.AccountID == account {
			r.publishQuotaPause()
		}
	}
}

func (r *chatRoom) publishQuotaPause() {
	r.mu.Lock()
	open := len(r.conns) > 0
	r.mu.Unlock()
	if open {
		r.broadcast(r.quotaPauseMessage())
	}
}

func (r *chatRoom) quotaPauseMessage() map[string]any {
	msg := map[string]any{"type": "quota_pause", "pause": nil}
	if p, ok := r.srv.quotaPaused(r.sessID); ok {
		msg["pause"] = p
	}
	return msg
}

// queueQuotaResume queues a "继续" after the request the pause stopped, in its
// thread and with its settings. Follow-ups the user queued in that thread
// already carry on instead, and a deleted thread is left alone.
func (r *chatRoom) queueQuotaResume(requestID string) {
	s := r.srv
	sess, ok := s.store.Get(r.sessID)
	if !ok || requestID == "" {
		return
	}
	u, ok := s.store.GetUser(sess.User)
	if !ok {
		return
	}
	r.submitMu.Lock()
	defer r.submitMu.Unlock()
	stopped, err := s.store.ChatRequest(u, sess.ID, requestID)
	if err != nil || stopped.Request == nil || s.queueWaiting(sess.ID, stopped.ThreadID) {
		return
	}
	in := *stopped.Request
	in.Text, in.Attachments, in.Delivery, in.Scope = quotaResumeText, nil, store.DeliveryQueue, s.chatRequestScope(u)
	r.fileMu.Lock()
	exists := s.threadExists(sess, in.ThreadID)
	r.fileMu.Unlock()
	if in.Scope == "" || !exists {
		return
	}
	c, fresh, err := s.store.QueueChatRequest(u, sess.ID, newOperationID(), in)
	if err != nil || !fresh {
		log.Printf("five-hour pause %s: queue resume: %v", sess.ID, err)
		return
	}
	r.broadcast(protocol.Event(c))
	r.kickQueue()
}
