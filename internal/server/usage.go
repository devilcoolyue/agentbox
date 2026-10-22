package server

import (
	"log"
	"time"

	"agentbox/internal/store"
	"agentbox/internal/usage"
)

// Publish only after the settlement transaction commits. Historical views read
// this same amount from SQLite, so editing pricing cannot change old answers.
// running marks a mid-turn total: requests settled so far, before the rest of
// the turn (sub-agents, compaction) is booked at its end.
func (r *chatRoom) publishTurnCost(threadID, turnID string, running bool) {
	cost := store.ChatTurnCost{TurnID: turnID, Source: "unknown"}
	values, err := r.srv.store.ChatTurnCosts(r.sessID, threadID, []string{turnID})
	if err != nil {
		log.Printf("chat cost %s: %v", r.sessID, err)
	} else if value, ok := values[turnID]; ok {
		cost = value
	}
	msg := map[string]any{"type": "turn_cost", "cost": cost}
	if running {
		msg["running"] = true
	}
	r.broadcast(msg)
}

type usageTally = usage.Tally

func (r *chatRoom) flushUsage(t *usageTally, wall time.Duration) int {
	return r.srv.usageService().Flush(t, wall)
}
func (r *chatRoom) recordUsage(base store.UsageEvent, line []byte, wall time.Duration) int {
	return r.srv.usageService().Record(base, line, wall)
}
func (s *Server) usageService() *usage.Service {
	s.usageOnce.Do(func() {
		s.usage = usage.New(s.workContext(), s.cfg, s.store)
		s.usage.OnTerminalCache(s.noteTerminalCache)
	})
	return s.usage
}
