package workspace

import (
	"context"
	"errors"

	"agentbox/internal/store"
)

type creationStore interface {
	WorkspaceCreation(store.User, string) (store.WorkspaceCreation, error)
	CompleteWorkspaceCreation(store.User, string) (store.Session, error)
}

// CreateReserved serializes resumable filesystem preparation with all other
// lifecycle actions. A receipt pins the ID/model before any directory writes.
func (s *Service) CreateReserved(ctx context.Context, u store.User, id string) (store.Session, error) {
	db, ok := s.store.(creationStore)
	if !ok {
		return store.Session{}, errors.New("creation receipts unavailable")
	}
	c, err := db.WorkspaceCreation(u, id)
	if err != nil {
		return store.Session{}, err
	}
	lock := s.lock(c.Session.ID)
	if err = lock.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer lock.release()
	c, err = db.WorkspaceCreation(u, id)
	if err != nil {
		return store.Session{}, err
	}
	if c.State == "abandoned" {
		return store.Session{}, store.ErrCreationGone
	}
	if c.State != "reserved" {
		if existing, ok := s.store.Get(c.Session.ID); ok && existing.User == u.Name {
			return existing, nil
		}
		return store.Session{}, store.ErrCreationGone
	}
	if _, err = s.account(c.Session); err != nil {
		return store.Session{}, err
	}
	if err = ctx.Err(); err != nil {
		return store.Session{}, err
	}
	if err = s.prepareDirectories(c.Session); err != nil {
		return store.Session{}, err
	}
	if _, err = s.account(c.Session); err != nil {
		return store.Session{}, err
	}
	if err = ctx.Err(); err != nil {
		return store.Session{}, err
	}
	return db.CompleteWorkspaceCreation(u, id)
}
