package server

import (
	"context"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
)

func (s *Server) ensureClaudeCred(ctx context.Context, acct config.Account, force bool) (credentials.ClaudeCred, error) {
	return s.credentialService().EnsureClaude(ctx, acct, force)
}
func (s *Server) credentialService() *credentials.Service {
	s.credentialsOnce.Do(func() {
		s.credentials = credentials.New(s.cfg, s.store, s.acctClient, claudeOAuthClientID, func() string { return claudeOAuthToken })
	})
	return s.credentials
}
