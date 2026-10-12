package server

import (
	"agentbox/internal/store"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Live output is disposable. Reconnection reads durable events/history; it
// never claims or reruns a step. A slow browser cannot block model settlement.
func (s *Server) emitCollaboration(run string, value any) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 256<<10 {
		return
	}
	j := &s.collaborationJobs
	j.mu.Lock()
	defer j.mu.Unlock()
	for ch := range j.listeners[run] {
		select {
		case ch <- raw:
		default:
		}
	}
}
func (s *Server) handleCollaborationStream(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if run.State == store.CollabDeleted {
		writeCollaborationError(w, r, store.ErrChatGone)
		return
	}
	ch := make(chan []byte, 8)
	j := &s.collaborationJobs
	j.mu.Lock()
	if j.listeners == nil {
		j.listeners = map[string]map[chan []byte]bool{}
	}
	if j.listeners[run.ID] == nil {
		j.listeners[run.ID] = map[chan []byte]bool{}
	}
	j.listeners[run.ID][ch] = true
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		delete(j.listeners[run.ID], ch)
		if len(j.listeners[run.ID]) == 0 {
			delete(j.listeners, run.ID)
		}
		j.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(raw []byte) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, e := fmt.Fprintf(w, "data: %s\n\n", raw)
		if e != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !write([]byte(`{"type":"connected"}`)) {
		return
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case raw := <-ch:
			current, e := s.store.Collaboration(u, run.ID)
			if e != nil || current.State == store.CollabDeleted {
				return
			}
			if !write(raw) {
				return
			}
		case <-tick.C:
			current, ok := s.store.TokenUser(bearerToken(r))
			if !ok || current.Name != u.Name || !current.CreatedAt.Equal(u.CreatedAt) {
				return
			}
			currentRun, e := s.store.Collaboration(u, run.ID)
			if e != nil || currentRun.State == store.CollabDeleted || !s.cfg.GetCollaborationEnabled() {
				return
			}
			if !write([]byte(`{"type":"heartbeat"}`)) {
				return
			}
		}
	}
}
