package server

import (
	"context"
	"log"
	"sync"
	"time"

	"agentbox/internal/store"
)

// reaperInterval is how often the idle reaper sweeps running sessions. The
// threshold itself is the admin-configurable idle_timeout_min (minutes), read
// fresh each sweep so edits take effect without a restart.
const reaperInterval = time.Minute

// idleTimeout returns the current idle-stop threshold; a non-positive value
// means the reaper is disabled. Stopping only halts the container; the
// workspace/home live on the host, so the next chat turn or terminal connect
// transparently brings it back up.
func (s *Server) idleTimeout() time.Duration {
	return time.Duration(s.cfg.GetIdleTimeoutMin()) * time.Minute
}

// activity tracks per-session liveness for the idle reaper. A session is kept
// alive while any long-lived use holds it (holds > 0, e.g. an attached
// terminal or an in-flight chat turn); once the last hold is released,
// lastSeen begins the idle countdown. Momentary hits (a start call) just bump
// lastSeen via touch.
type activity struct {
	mu   sync.Mutex
	seen map[string]*sessActivity
}

type sessActivity struct {
	lastSeen time.Time
	holds    int
}

func newActivity() *activity { return &activity{seen: map[string]*sessActivity{}} }

// entry returns the record for id, creating it. Caller holds a.mu.
func (a *activity) entry(id string) *sessActivity {
	e := a.seen[id]
	if e == nil {
		e = &sessActivity{lastSeen: time.Now()}
		a.seen[id] = e
	}
	return e
}

// touch records momentary activity, resetting the idle countdown.
func (a *activity) touch(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entry(id).lastSeen = time.Now()
}

// hold marks a long-lived use that must keep the container alive until the
// matching release. Pair it with `defer release`.
func (a *activity) hold(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entry(id)
	e.holds++
	e.lastSeen = time.Now()
}

func (a *activity) release(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entry(id)
	if e.holds > 0 {
		e.holds--
	}
	e.lastSeen = time.Now()
}

// forget drops a session's record (on delete) so the map doesn't leak.
func (a *activity) forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.seen, id)
}

// reapable reports whether id has been idle at least d and holds nothing, so
// the reaper may stop it. A session with no record yet is seeded and spared:
// containers found running at boot thus get a full idle window before their
// first eligible sweep, rather than being stopped immediately.
func (a *activity) reapable(id string, d time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.seen[id]
	if e == nil {
		a.seen[id] = &sessActivity{lastSeen: time.Now()}
		return false
	}
	if e.holds > 0 {
		return false
	}
	return time.Since(e.lastSeen) >= d
}

// idleReaper stops containers of sessions that have gone idle, freeing their
// memory/CPU reservation. Runs for the life of the process.
func (s *Server) idleReaper() {
	for {
		time.Sleep(reaperInterval)
		s.reapIdle()
	}
}

func (s *Server) reapIdle() {
	d := s.idleTimeout()
	if d <= 0 {
		return // admin disabled auto-stop
	}
	for _, sess := range s.store.All() {
		if sess.Status != store.StatusRunning || sess.ContainerID == "" {
			continue
		}
		if !s.idle.reapable(sess.ID, d) {
			continue
		}
		s.stopIdle(sess.ID, d)
	}
}

// stopIdle stops one idle session's container under its start lock, so it can
// never race a concurrent bring-up: startSession holds the same lock, and a
// turn/terminal that begins after this returns simply restarts the container.
func (s *Server) stopIdle(id string, d time.Duration) {
	lock := s.startLock(id)
	lock.Lock()
	defer lock.Unlock()

	// Re-read under the lock: state may have changed since the sweep, and a
	// hold may have appeared just before we acquired it.
	sess, ok := s.store.Get(id)
	if !ok || sess.Status != store.StatusRunning || sess.ContainerID == "" {
		return
	}
	if !s.idle.reapable(id, d) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.dock.Stop(ctx, sess.ContainerID); err != nil {
		log.Printf("idle reaper stop %s: %v", id, err)
		return
	}
	if _, err := s.store.Update(id, func(x *store.Session) {
		x.Status = store.StatusStopped
		x.StopReason = store.StopIdle // 让前端把「休眠」与用户手动停止区分开
	}); err != nil {
		log.Printf("idle reaper mark %s: %v", id, err)
	}
	log.Printf("idle reaper: stopped %s after %s idle", id, d)
}
