package server

import (
	"context"
	"log"
	"sync"
	"time"

	"agentbox/internal/store"
)

type collaborationJobs struct {
	listeners  map[string]map[chan []byte]bool
	interrupts map[string]func()
	mu         sync.Mutex
	running    map[string]bool
	again      map[string]bool
	cancels    map[string]context.CancelFunc
}

func (s *Server) cancelCollaborationJob(id string) {
	j := &s.collaborationJobs
	j.mu.Lock()
	cancel := j.cancels[id]
	interrupt := j.interrupts[id]
	j.mu.Unlock()
	if interrupt != nil {
		interrupt()
		return
	}
	if cancel != nil {
		cancel()
	}
}

// Kicks are coalesced, not dropped while a task is between two steps. Store
// claims still fence every provider call; this map only avoids duplicate work.
func (s *Server) kickCollaboration(u store.User, id string) {
	j := &s.collaborationJobs
	j.mu.Lock()
	if j.running == nil {
		j.running = map[string]bool{}
	}
	if j.again == nil {
		j.again = map[string]bool{}
	}
	if j.cancels == nil {
		j.cancels = map[string]context.CancelFunc{}
	}
	if j.running[id] {
		j.again[id] = true
		j.mu.Unlock()
		return
	}
	j.running[id] = true
	j.mu.Unlock()
	if !s.spawn(func() {
		for {
			ctx, cancel := context.WithCancel(s.workContext())
			j.mu.Lock()
			j.cancels[id] = cancel
			j.mu.Unlock()
			err := s.executeCollaboration(ctx, u, id, func(value any) { s.emitCollaboration(id, value) })
			cancel()
			if err != nil {
				log.Printf("collaboration execution requires inspection run=%s", id)
			}
			j.mu.Lock()
			delete(j.cancels, id)
			again := j.again[id]
			delete(j.again, id)
			if !again || s.workContext().Err() != nil {
				delete(j.running, id)
				j.mu.Unlock()
				return
			}
			j.mu.Unlock()
		}
	}) {
		j.mu.Lock()
		delete(j.running, id)
		delete(j.again, id)
		j.mu.Unlock()
	}
}

func (s *Server) collaborationWaitLoop() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-s.workContext().Done():
			return
		case now := <-tick.C:
			if s.cfg.GetCollaborationEnabled() {
				if err := s.store.ExpireCollaborationWaits(now); err != nil {
					log.Print("collaboration wait expiry failed")
				}
			}
		}
	}
}
