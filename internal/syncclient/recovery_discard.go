package syncclient

import (
	"context"
	"encoding/json"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

func completedRecoveryBatch(batch Batch) bool {
	if batch.Resolution != nil && batch.Resolution.Action != "finish" {
		return false
	}
	for _, op := range batch.Operations {
		if !completedRecoveryOperation(batch, op) {
			return false
		}
	}
	return true
}

// A reconciled finish preserves the original started audit state. Its durable
// whole-tree verification is just as authoritative as the normal commit path.
func completedRecoveryOperation(batch Batch, op SavedOperation) bool {
	return op.Status == "verified" || op.Status == "started" && batch.Resolution != nil && batch.Resolution.Action == "finish" && syncproto.ValidHash(batch.Resolution.ReviewDigest) && syncproto.ValidHash(batch.Resolution.LocalDigest) && syncproto.ValidHash(batch.Resolution.RemoteDigest)
}

// DiscardLocalRecovery is explicit, per-file, revision-bound and resumable.
// Retain the operation and original reference after deletion. This never
// changes the sync baseline or retires a remote operation ID.
func (e *Engine) DiscardLocalRecovery(ctx context.Context, id, batchID, operationID string, revision int64) (SavedBinding, error) {
	saved, _, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return SavedBinding{}, err
	}
	if !syncproto.ValidOperationID(batchID) || !syncproto.ValidOperationID(operationID) {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	if saved.Revision != revision {
		return SavedBinding{}, ErrStateChanged
	}
	if saved.Pending != nil {
		return SavedBinding{}, ErrPending
	}
	root, err := syncfs.OpenContext(ctx, saved.Directory)
	if err != nil {
		return SavedBinding{}, err
	}
	defer root.Close()
	identity, err := root.Identity()
	if err != nil {
		return SavedBinding{}, err
	}
	if identity != saved.Binding.LocalID {
		return SavedBinding{}, ErrBinding
	}
	// First transaction commits the user's intent before any unlink. The second
	// holds SQLite's write lock across unlink/final state so competing sidecars
	// cannot start a new batch using a stale revision during disposal.
	saved, err = e.State.discardRecoveryStep(ctx, saved, batchID, operationID, nil)
	if err != nil {
		return SavedBinding{}, err
	}
	return e.State.discardRecoveryStep(ctx, saved, batchID, operationID, root)
}

func (s *StateStore) discardRecoveryStep(ctx context.Context, saved SavedBinding, batchID, operationID string, root *syncfs.Root) (SavedBinding, error) {
	if err := s.root.CheckIdentity(); err != nil {
		return SavedBinding{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SavedBinding{}, err
	}
	defer tx.Rollback()
	var actual int64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM bindings WHERE id=?", saved.ID).Scan(&actual); err != nil {
		return SavedBinding{}, err
	}
	if actual != saved.Revision {
		return SavedBinding{}, ErrStateChanged
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, "SELECT state FROM batches WHERE binding=? AND id=?", saved.ID, batchID).Scan(&raw); err != nil {
		return SavedBinding{}, err
	}
	var batch Batch
	if len(raw) > maxStateJSON || json.Unmarshal(raw, &batch) != nil || batch.ID != batchID {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	if err = validateRecoveryBatch(batch, saved.Binding); err != nil {
		return SavedBinding{}, err
	}
	if !completedRecoveryBatch(batch) {
		return SavedBinding{}, ErrPending
	}
	index := -1
	for i, op := range batch.Operations {
		if op.ID == operationID {
			index = i
			break
		}
	}
	if index < 0 {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	op := &batch.Operations[index]
	if op.Recovery == nil || remoteKind(batch.Plan.Operations[index].Kind) != "" || op.RecoveryState == "discarded" {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	if root == nil {
		op.RecoveryState = "discarding"
	} else {
		if op.RecoveryState != "discarding" {
			return SavedBinding{}, syncproto.ErrInvalid
		}
		if err = root.DiscardRecovery(ctx, *op.Recovery); err != nil {
			return SavedBinding{}, err
		}
		op.RecoveryState = "discarded"
	}
	raw, err = json.Marshal(batch)
	if err != nil {
		return SavedBinding{}, err
	}
	if len(raw) > maxStateJSON {
		return SavedBinding{}, syncproto.ErrLimit
	}
	if _, err = tx.ExecContext(ctx, "UPDATE batches SET state=? WHERE binding=? AND id=?", raw, saved.ID, batchID); err != nil {
		return SavedBinding{}, err
	}
	saved.Revision++
	raw, err = json.Marshal(saved)
	if err != nil {
		return SavedBinding{}, err
	}
	if err = checkStateCapacity(tx, saved.ID, len(raw)); err != nil {
		return SavedBinding{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE bindings SET revision=?,state=? WHERE id=?", saved.Revision, raw, saved.ID); err != nil {
		return SavedBinding{}, err
	}
	if err = s.root.CheckIdentity(); err != nil {
		return SavedBinding{}, err
	}
	if err = tx.Commit(); err != nil {
		return SavedBinding{}, err
	}
	return saved, nil
}
