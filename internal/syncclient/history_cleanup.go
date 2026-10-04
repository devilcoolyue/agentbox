package syncclient

import (
	"context"
	"encoding/json"

	"agentbox/internal/syncproto"
)

// Only completed history with no overwritten/deleted file content may be
// removed. Pending, uncertain, replanned and abandoned batches stay indexed.
// Server receipts and all actual files remain untouched.
func historyCleanable(batch Batch, pending bool) bool {
	if pending || batch.Resolution != nil && batch.Resolution.Action != "finish" {
		return false
	}
	for i, op := range batch.Operations {
		if remoteKind(batch.Plan.Operations[i].Kind) != "" && op.RemoteRetirement != "retired" {
			return false
		}
		if !completedRecoveryOperation(batch, op) || op.Recovery != nil {
			return false
		}
		before := batch.Plan.Operations[i].Before
		if before != nil && before.Kind == "file" {
			return false
		}
	}
	return true
}

func (e *Engine) CleanupHistory(ctx context.Context, id, batchID string, revision int64) (SavedBinding, error) {
	saved, _, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return SavedBinding{}, err
	}
	if !syncproto.ValidOperationID(batchID) {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	if saved.Revision != revision {
		return SavedBinding{}, ErrStateChanged
	}
	if saved.Pending != nil {
		return SavedBinding{}, ErrPending
	}
	if err = e.State.root.CheckIdentity(); err != nil {
		return SavedBinding{}, err
	}
	tx, err := e.State.db.BeginTx(ctx, nil)
	if err != nil {
		return SavedBinding{}, err
	}
	defer tx.Rollback()
	var actual int64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM bindings WHERE id=?", id).Scan(&actual); err != nil {
		return SavedBinding{}, err
	}
	if actual != revision {
		return SavedBinding{}, ErrStateChanged
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, "SELECT state FROM batches WHERE binding=? AND id=?", id, batchID).Scan(&raw); err != nil {
		return SavedBinding{}, err
	}
	var batch Batch
	if len(raw) > maxStateJSON || json.Unmarshal(raw, &batch) != nil || batch.ID != batchID {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	if err = validateRecoveryBatch(batch, saved.Binding); err != nil {
		return SavedBinding{}, err
	}
	if !historyCleanable(batch, false) {
		return SavedBinding{}, ErrPending
	}
	saved.Revision++
	raw, err = json.Marshal(saved)
	if err != nil {
		return SavedBinding{}, err
	}
	var verified SavedBinding
	if err = decodeState(raw, &verified); err != nil {
		return SavedBinding{}, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM batches WHERE binding=? AND id=?", id, batchID); err != nil {
		return SavedBinding{}, err
	}
	if err = checkStateCapacity(tx, id, len(raw)); err != nil {
		return SavedBinding{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE bindings SET revision=?,state=? WHERE id=?", saved.Revision, raw, id); err != nil {
		return SavedBinding{}, err
	}
	if err = e.State.root.CheckIdentity(); err != nil {
		return SavedBinding{}, err
	}
	if err = tx.Commit(); err != nil {
		return SavedBinding{}, err
	}
	return saved, nil
}
