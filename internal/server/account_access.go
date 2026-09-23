package server

import (
	"fmt"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

var errAccountAccess = jsonError("当前用户无权使用该账号，请联系管理员调整账号使用范围")

// Use the session owner's current role, never the role of an unrelated caller.
func (s *Server) canUseAccount(acct config.Account, user string) bool {
	u, _ := s.store.GetUser(user)
	return acct.CanUse(user, u.Role == store.RoleAdmin)
}

func (s *Server) sessionAccount(sess store.Session) (config.Account, error) {
	a, ok := s.cfg.Account(sess.AccountID)
	if !ok {
		return config.Account{}, errAccountGone
	}
	if !s.canUseAccount(a, sess.User) {
		return config.Account{}, errAccountAccess
	}
	return a, nil
}

// Config loading permits names not yet restored into SQLite. Administrative
// HTTP edits require existing users so a typo cannot silently grant future access.
func (s *Server) validateAccountAccess(a *config.AccountAccess) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a != nil {
		for _, name := range a.Users {
			if _, ok := s.store.GetUser(name); !ok {
				return fmt.Errorf("用户 %q 不存在", name)
			}
		}
	}
	return nil
}
