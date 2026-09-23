package server

import "time"

const reaperInterval = time.Minute

func (s *Server) idleReaper() {
	for waitInterval(s.workContext(), reaperInterval) {
		s.workspaces().Reap(s.workContext(), time.Duration(s.cfg.GetIdleTimeoutMin())*time.Minute)
	}
}
