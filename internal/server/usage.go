package server

import (
	"time"

	"agentbox/internal/store"
	"agentbox/internal/usage"
)

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
