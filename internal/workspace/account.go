package workspace

import (
	"context"
	"errors"

	"agentbox/internal/store"
)

// ErrAccountAgent rejects an account whose agent differs from the workspace's:
// the home layout, history and CLI are per agent, so only same-type accounts swap.
var ErrAccountAgent = errors.New("account agent does not match workspace")

// ReleaseFunc hands the stopped workspace's credentials back to its current
// account and calls rebind while that account is still locked
// (credentials.Service.Release).
type ReleaseFunc func(ctx context.Context, sess store.Session, rebind func() error) error

// SwitchAccount rebinds a workspace to another account of the same agent.
//
// The container is stopped first: CLIs still running in it (terminal, title
// generation) hold the previous login in memory and would write refreshed
// tokens back into the home after the handover. Stopping also ends terminals
// and the remote browser; files, chat history and the container itself stay.
// The workspace is left stopped and the next start seeds the new account.
// Callers keep chat turns and imports out for the duration.
func (s *Service) SwitchAccount(ctx context.Context, id, accountID string, release ReleaseFunc) (store.Session, error) {
	l := s.lock(id)
	if err := l.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer l.release()
	cur, ok := s.store.Get(id)
	if !ok {
		return store.Session{}, ErrSessionGone
	}
	if cur.AccountID == accountID {
		return cur, nil
	}
	// Re-check under the lock with the owner's current access.
	next := cur
	next.AccountID = accountID
	acct, err := s.account(next)
	if err != nil {
		return store.Session{}, err
	}
	if acct.Type != cur.Agent {
		return store.Session{}, ErrAccountAgent
	}
	if cur.ContainerID != "" {
		if err := s.dock.Stop(ctx, cur.ContainerID); err != nil {
			return store.Session{}, err
		}
	}
	// Record the stop on its own: if the handover fails the container is still
	// down, and the workspace keeps its previous account.
	cur, err = s.store.Update(id, func(x *store.Session) { x.Status = store.StatusStopped; x.StopReason = "" })
	if err != nil {
		return store.Session{}, err
	}
	var updated store.Session
	err = release(ctx, cur, func() error {
		var err error
		// A model the new account does not offer would be rejected by every
		// turn; start from the new account's default instead.
		updated, err = s.store.Update(id, func(x *store.Session) { x.AccountID = accountID; x.DefaultModel = acct.ResolveModel(x.DefaultModel) })
		return err
	})
	if err != nil {
		return store.Session{}, err
	}
	return updated, nil
}
