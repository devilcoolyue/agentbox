package workspace

import "context"

// MaintainUser reclaims one user's rebuildable caches with server-owned container
// starts held off, so that user's workspaces cannot come up mid-cleanup. A running
// workspace of that user skips the pass; other users' workspaces do not, because
// each user's cache is mounted only into their own containers. It never stops
// users' containers to make maintenance possible.
func (s *Service) MaintainUser(ctx context.Context, user string, work func() error) error {
	if err := s.starts.acquire(ctx); err != nil {
		return err
	}
	defer s.starts.release()
	for _, sess := range s.store.All() {
		if sess.ContainerID == "" || sess.User != user {
			continue
		}
		running, err := s.dock.Running(ctx, sess.ContainerID)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}
