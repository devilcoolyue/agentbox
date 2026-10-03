package syncclient

import (
	"context"
	"encoding/json"
	"net/http"

	"agentbox/internal/syncproto"
)

type RemoteCleanupItem struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"`
	State string `json:"state"`
}
type RemoteCleanupReview struct {
	BindingID string              `json:"binding_id"`
	BatchID   string              `json:"batch_id"`
	Revision  int64               `json:"revision"`
	Digest    string              `json:"digest"`
	Items     []RemoteCleanupItem `json:"items"`
}

func (r *Remote) RecoveryStorage(ctx context.Context, workspace string) (syncproto.RecoveryStorage, error) {
	var result syncproto.RecoveryStorage
	response, err := r.request(ctx, http.MethodGet, workspace, "storage", nil, nil, "")
	if err != nil {
		return result, err
	}
	if err = decodeRemote(response, 16<<10, &result); err != nil {
		return result, err
	}
	if result.Validate() != nil {
		return syncproto.RecoveryStorage{}, ErrProtocol
	}
	return result, nil
}

func (r *Remote) retireOperation(ctx context.Context, workspace string, status syncproto.OperationStatus) (syncproto.OperationStatus, error) {
	var result syncproto.OperationStatus
	request := syncproto.RetireOperationRequest{Device: status.Device, Digest: status.Digest, Confirmation: status.RetirementConfirmation}
	if request.Validate() != nil || status.Validate(status.Operation.ID) != nil {
		return result, syncproto.ErrInvalid
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	response, err := r.request(ctx, http.MethodPost, workspace, "operations/"+status.Operation.ID+"/retire", nil, raw, "")
	if err != nil {
		return result, err
	}
	if err = decodeRemote(response, 16<<10, &result); err != nil {
		return result, err
	}
	if result.Validate(status.Operation.ID) != nil || result.Digest != status.Digest || result.Device != status.Device || result.Project != status.Project || result.Retirement != "retired" || result.RetirementConfirmation != status.RetirementConfirmation || result.Operation.Recovery {
		return syncproto.OperationStatus{}, ErrProtocol
	}
	return result, nil
}

// Capability absence is distinct from a failed storage read. Never probe or
// send old operation IDs to another instance at the same URL.
func (e *Engine) cleanupRemote(ctx context.Context, saved SavedBinding) (*Remote, error) {
	identity, err := e.Remote.Identity(ctx)
	if err != nil {
		return nil, err
	}
	if identity.ServerID != saved.Binding.ServerID || identity.User != saved.Binding.User || e.Remote.base.String() != saved.Binding.Server {
		return nil, ErrBinding
	}
	if identity.Features["sync_recovery_gc"] != 1 {
		return nil, ErrSyncUnavailable
	}
	pinned := *e.Remote
	pinned.serverID = identity.ServerID
	return &pinned, nil
}

func (e *Engine) ReviewRemoteCleanup(ctx context.Context, id, batchID string, revision int64) (RemoteCleanupReview, error) {
	review, _, err := e.reviewRemoteCleanup(ctx, id, batchID, revision)
	return review, err
}

func (e *Engine) reviewRemoteCleanup(ctx context.Context, id, batchID string, revision int64) (RemoteCleanupReview, []syncproto.OperationStatus, error) {
	fail := func(err error) (RemoteCleanupReview, []syncproto.OperationStatus, error) {
		return RemoteCleanupReview{}, nil, err
	}
	saved, _, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return fail(err)
	}
	if saved.Revision != revision {
		return fail(ErrStateChanged)
	}
	if saved.Pending != nil {
		return fail(ErrPending)
	}
	if !syncproto.ValidOperationID(batchID) {
		return fail(syncproto.ErrInvalid)
	}
	batch, err := e.State.History(id, batchID)
	if err != nil {
		return fail(err)
	}
	if err = validateRecoveryBatch(batch, saved.Binding); err != nil {
		return fail(err)
	}
	if !completedRecoveryBatch(batch) {
		return fail(ErrPending)
	}
	remote, err := e.cleanupRemote(ctx, saved)
	if err != nil {
		return fail(err)
	}
	device, err := e.State.Device()
	if err != nil {
		return fail(err)
	}
	report := RemoteCleanupReview{BindingID: id, BatchID: batchID, Revision: revision, Items: []RemoteCleanupItem{}}
	statuses := []syncproto.OperationStatus{}
	for i, op := range batch.Operations {
		plan := batch.Plan.Operations[i]
		kind := remoteKind(plan.Kind)
		if kind == "" {
			continue
		}
		if len(statuses) >= 1000 {
			return fail(syncproto.ErrLimit)
		}
		status, err := remote.Operation(ctx, saved.Binding.Workspace, op.ID)
		if err != nil {
			return fail(err)
		}
		mutation := syncproto.Mutation{Version: syncproto.Version, ID: op.ID, Project: saved.Binding.Project, Revision: batch.ProjectRevision, RulesHash: batch.Remote.RulesHash, Device: device, Generation: "preview", Path: plan.Path, Kind: kind, Before: plan.Before, After: plan.After}
		digest, err := mutation.Digest()
		if err != nil {
			return fail(err)
		}
		if !matchesReceipt(status, plan) || status.Operation.Status != "applied" || status.Digest != digest || status.Device != device || status.Project != saved.Binding.Project || !syncproto.ValidHash(status.RetirementConfirmation) {
			return fail(ErrProtocol)
		}
		// A local retired audit row must never silently refer to a resurrected
		// server copy (e.g. an improperly copied backup with the old identity).
		if op.RemoteRetirement == "retired" && status.Retirement != "retired" {
			return fail(ErrProtocol)
		}
		item := RemoteCleanupItem{ID: op.ID, Path: plan.Path, Kind: kind, State: status.Retirement}
		if plan.Before != nil && plan.Before.Kind == "file" {
			item.Bytes = plan.Before.Size
		}
		report.Items = append(report.Items, item)
		statuses = append(statuses, status)
	}
	if len(statuses) == 0 {
		return fail(syncproto.ErrInvalid)
	}
	// All mutable availability flags are excluded. Retrying a lost response
	// still refers to exactly the originally confirmed operations and old bytes.
	type identity struct{ ID, Digest, Confirmation string }
	identities := make([]identity, 0, len(statuses))
	for _, status := range statuses {
		identities = append(identities, identity{status.Operation.ID, status.Digest, status.RetirementConfirmation})
	}
	raw, _ := json.Marshal(struct {
		Domain, Server, Binding, Batch string
		Revision                       int64
		Operations                     []identity
	}{"agentbox-remote-cleanup-v1", saved.Binding.ServerID, id, batchID, revision, identities})
	report.Digest = syncproto.HashBytes(raw)
	current, err := e.State.Load(id)
	if err != nil {
		return fail(err)
	}
	if current.Revision != revision {
		return fail(ErrStateChanged)
	}
	return report, statuses, nil
}

func (e *Engine) CleanupRemoteRecovery(ctx context.Context, id, batchID string, revision int64, confirmation string) (SavedBinding, error) {
	report, statuses, err := e.reviewRemoteCleanup(ctx, id, batchID, revision)
	if err != nil {
		return SavedBinding{}, err
	}
	if report.Digest != confirmation {
		return SavedBinding{}, ErrStateChanged
	}
	saved, err := e.State.Load(id)
	if err != nil {
		return SavedBinding{}, err
	}
	if saved.Revision != revision {
		return SavedBinding{}, ErrStateChanged
	}
	remote, err := e.cleanupRemote(ctx, saved)
	if err != nil {
		return SavedBinding{}, err
	}
	for _, status := range statuses {
		// Persist consent before the network write. The per-operation transaction
		// below holds the SQLite write lock while retiring, so another sidecar
		// cannot begin a new local batch across this boundary.
		saved, err = e.State.remoteCleanupStep(ctx, saved, batchID, status, nil)
		if err != nil {
			return saved, err
		}
		saved, err = e.State.remoteCleanupStep(ctx, saved, batchID, status, remote)
		if err != nil {
			return saved, err
		}
	}
	return saved, nil
}

func (s *StateStore) remoteCleanupStep(ctx context.Context, saved SavedBinding, batchID string, status syncproto.OperationStatus, remote *Remote) (SavedBinding, error) {
	if err := s.root.CheckIdentity(); err != nil {
		return SavedBinding{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SavedBinding{}, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, "SELECT state FROM bindings WHERE id=?", saved.ID).Scan(&raw); err != nil {
		return SavedBinding{}, err
	}
	var current SavedBinding
	if err = decodeState(raw, &current); err != nil {
		return SavedBinding{}, err
	}
	if current.Revision != saved.Revision {
		return SavedBinding{}, ErrStateChanged
	}
	if current.Pending != nil {
		return SavedBinding{}, ErrPending
	}
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
		if op.ID == status.Operation.ID {
			index = i
			break
		}
	}
	if index < 0 || remoteKind(batch.Plan.Operations[index].Kind) == "" || !matchesReceipt(status, batch.Plan.Operations[index]) {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	op := &batch.Operations[index]
	if op.RemoteRetirement == "retired" {
		return saved, nil
	}
	if remote == nil {
		op.RemoteRetirement = "retiring"
	} else {
		if op.RemoteRetirement != "retiring" {
			return SavedBinding{}, syncproto.ErrInvalid
		}
		result, err := remote.retireOperation(ctx, saved.Binding.Workspace, status)
		if err != nil {
			return SavedBinding{}, err
		}
		if !matchesReceipt(result, batch.Plan.Operations[index]) {
			return SavedBinding{}, ErrProtocol
		}
		op.RemoteRetirement = "retired"
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
