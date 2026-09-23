package workspace

import (
	"sync"
	"time"
)

// activity tracks per-session liveness for the idle reaper. A session is kept
// alive while any long-lived use holds it (holds > 0, e.g. an attached
// terminal or an in-flight chat turn); once the last hold is released,
// lastSeen begins the idle countdown. Momentary hits (a start call) just bump
// lastSeen via touch.
type Activity struct {
	mu   sync.Mutex
	seen map[string]*sessActivity
}

type sessActivity struct {
	lastSeen time.Time
	holds    int
}

func NewActivity() *Activity { return &Activity{seen: map[string]*sessActivity{}} }

// entry returns the record for id, creating it. Caller holds a.mu.
func (a *Activity) entry(id string) *sessActivity {
	e := a.seen[id]
	if e == nil {
		e = &sessActivity{lastSeen: time.Now()}
		a.seen[id] = e
	}
	return e
}

// touch records momentary activity, resetting the idle countdown.
func (a *Activity) Touch(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entry(id).lastSeen = time.Now()
}

// hold marks a long-lived use that must keep the container alive until the
// matching release. Pair it with `defer release`.
func (a *Activity) Hold(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entry(id)
	e.holds++
	e.lastSeen = time.Now()
}

func (a *Activity) Release(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.seen[id]
	if e == nil {
		return
	}
	if e.holds > 0 {
		e.holds--
	}
	e.lastSeen = time.Now()
}

// forget drops a session's record (on delete) so the map doesn't leak.
func (a *Activity) Forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.seen, id)
}

// reapable reports whether id has been idle at least d and holds nothing, so
// the reaper may stop it. A session with no record yet is seeded and spared:
// containers found running at boot thus get a full idle window before their
// first eligible sweep, rather than being stopped immediately.
func (a *Activity) Reapable(id string, d time.Duration) bool {
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
