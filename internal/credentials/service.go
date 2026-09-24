// Package credentials owns account credential reads, rotation and propagation.
// It has no dependency on HTTP handlers or the server package.
package credentials

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

type Sessions interface {
	All() []store.Session
	GetUser(string) (store.User, bool)
}
type Service struct {
	cfg      *config.Config
	sessions Sessions
	client   func(config.Account, time.Duration) (*http.Client, error)
	clientID string
	tokenURL func() string
	locks    sync.Map
}

func New(cfg *config.Config, sessions Sessions, client func(config.Account, time.Duration) (*http.Client, error), clientID string, tokenURL func() string) *Service {
	return &Service{cfg: cfg, sessions: sessions, client: client, clientID: clientID, tokenURL: tokenURL}
}

// Lock serializes pool writes and refresh/sync for an account. Waiting is
// cancellable so shutdown need not wait behind a remote OAuth request.
func (s *Service) Lock(ctx context.Context, id string) (func(), error) {
	v, _ := s.locks.LoadOrStore(id, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-ch
			return nil, err
		}
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *Service) allowed(a config.Account, user string) bool {
	u, _ := s.sessions.GetUser(user)
	return a.CanUse(user, u.Role == store.RoleAdmin)
}
func (s *Service) openHome(sess store.Session) (*safefs.Root, error) {
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Sub(filepath.Join("users", sess.User, "sessions", sess.ID, "home"))
}
func readPoolFile(acct config.Account, name string) ([]byte, error) {
	if acct.CredentialsDir == "" {
		return nil, fmt.Errorf("account has no credential directory")
	}
	root, err := safefs.Open(acct.CredentialsDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	raw, _, err := readCredential(root, name)
	return raw, err
}
func (s *Service) Sync(ctx context.Context, acct config.Account, sess store.Session) error {
	release, err := s.Lock(ctx, acct.ID)
	if err != nil {
		return err
	}
	defer release()
	if sess.AccountID != acct.ID {
		return fmt.Errorf("credential account does not match session")
	}
	s.syncRotatingCred(acct, sess)
	return nil
}
func (s *Service) SyncAll(ctx context.Context) {
	for _, acct := range s.cfg.AccountList() {
		release, err := s.Lock(ctx, acct.ID)
		if err != nil {
			return
		}
		s.syncAcctCreds(acct)
		release()
	}
}

// broadcast is deliberately one-way after a successful refresh. Using the
// ordinary 2s mtime tolerance here could leave a just-seeded home on an obsolete
// refresh-token chain. Re-read authorization before each delivery.
func (s *Service) broadcast(acct config.Account) {
	current, ok := s.cfg.Account(acct.ID)
	if !ok {
		return
	}
	acct = current
	poolName, homeRel := agent.RotatingCredFile(acct.Type)
	pool, err := safefs.Open(acct.CredentialsDir)
	if err != nil {
		return
	}
	defer pool.Close()
	raw, info, err := readCredential(pool, poolName)
	if err != nil {
		return
	}
	for _, sess := range s.sessions.All() {
		current, ok = s.cfg.Account(acct.ID)
		if !ok || sess.AccountID != acct.ID || !s.allowed(current, sess.User) {
			continue
		}
		home, err := s.openHome(sess)
		if err != nil {
			continue
		}
		// Don't create a new credential tree in a home that has never been seeded.
		_, _, err = readCredential(home, filepath.FromSlash(homeRel))
		if err == nil {
			// Never publish subscription tokens while a session still targets a relay.
			if acct.Type == config.AgentCodex {
				configRaw, readErr := home.ReadAll(".codex/config.toml", credentialMaxBytes)
				if readErr == nil || os.IsNotExist(readErr) {
					var next []byte
					var poolConfig []byte
					poolConfig, readErr = readPoolFile(acct, "config.toml")
					if readErr == nil {
						next, readErr = codexConnectionConfig(configRaw, poolConfig)
					}
					if readErr == nil {
						_, readErr = home.WriteFile(".codex/config.toml", next, safefs.WriteOptions{Mode: 0600, Chown: true, UID: dockerx.AgentUID, GID: dockerx.AgentGID, BestEffortChown: true})
					}
				}
				if readErr != nil {
					log.Printf("credsync %s: Codex 配置切换失败: %v", sess.ID, readErr)
					home.Close()
					continue
				}
			}
			syncCredential(home, filepath.FromSlash(homeRel), raw, dockerx.AgentUID, info.ModTime())
		}
		home.Close()
	}
}
