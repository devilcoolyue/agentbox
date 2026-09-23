package server

import (
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

const credSyncInterval = 45 * time.Second

func (s *Server) credSyncLoop() {
	for waitInterval(s.workContext(), credSyncInterval) {
		s.credentialService().SyncAll(s.workContext())
	}
}
func (s *Server) syncRotatingCred(acct config.Account, sess store.Session) {
	_ = s.credentialService().Sync(s.workContext(), acct, sess)
}
