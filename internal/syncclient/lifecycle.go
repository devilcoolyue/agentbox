package syncclient

import "context"

// ArchiveBinding only retires the local mapping. It never deletes files,
// rewrites its identity or baseline, or discards an unfinished batch. A new
// binding starts without a baseline and requires a fresh preview/confirmation.
// The native UI supplies the exact revision shown in its confirmation dialog.
func (e *Engine) ArchiveBinding(ctx context.Context, id string, revision int64) (SavedBinding, error) {
	saved, _, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return SavedBinding{}, err
	}
	if saved.Archived || saved.Revision != revision {
		return SavedBinding{}, ErrStateChanged
	}
	if saved.Pending != nil {
		return SavedBinding{}, ErrPending
	}
	if err = ctx.Err(); err != nil {
		return SavedBinding{}, err
	}
	return e.State.change(id, revision, func(current *SavedBinding) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if current.Pending != nil {
			return ErrPending
		}
		current.Archived = true
		return nil
	}, false)
}
