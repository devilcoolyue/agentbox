package server

import (
	"log"
	"time"

	"agentbox/internal/store"
	"agentbox/internal/usage"
)

// Publish only after the settlement transaction commits. Historical views read
// this same amount from SQLite, so editing pricing cannot change old answers.
func (r *chatRoom) publishTurnCost(threadID, turnID string) {
	cost := store.ChatTurnCost{TurnID: turnID, Source: "unknown"}
	values, err := r.srv.store.ChatTurnCosts(r.sessID, threadID, []string{turnID})
	if err != nil {
		log.Printf("chat cost %s: %v", r.sessID, err)
	} else if value, ok := values[turnID]; ok {
		cost = value
	}
	r.broadcast(map[string]any{"type": "turn_cost", "cost": cost})
}

type usageTally = usage.Tally

func (r *chatRoom) flushUsage(t *usageTally, wall time.Duration) int {
	return r.srv.usageService().Flush(t, wall)
}
func (r *chatRoom) recordUsage(base store.UsageEvent, line []byte, wall time.Duration) int {
	return r.srv.usageService().Record(base, line, wall)
}
func (s *Server) usageService() *usage.Service {
	s.usageOnce.Do(func() { s.usage = usage.New(s.workContext(), s.cfg, s.store) })
	return s.usage
}
