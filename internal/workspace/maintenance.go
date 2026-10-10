package workspace

import "context"

// MaintainStopped prevents server-owned container starts while reclaiming
// rebuildable caches. Any running workspace causes this pass to be skipped.
// It never stops users' containers to make maintenance possible.
func (s *Service) MaintainStopped(ctx context.Context, work func() error) error {
	if err := s.starts.acquire(ctx); err != nil {
		return err
	}
	defer s.starts.release()
	for _, sess := range s.store.All() {
		if sess.ContainerID == "" {
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
