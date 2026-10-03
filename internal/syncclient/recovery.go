package syncclient

import (
	"context"
	"io"
	"path/filepath"
	"slices"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

type RecoveryBatch struct {
	RemoteOperations int            `json:"remote_operations"`
	RemoteRetired    int            `json:"remote_retired"`
	RemoteCleanable  bool           `json:"remote_cleanable"`
	Cleanable        bool           `json:"cleanable"`
	TotalFiles       int            `json:"total_files"`
	FileOffset       int            `json:"file_offset"`
	ID               string         `json:"id"`
	Pending          bool           `json:"pending"`
	Action           string         `json:"action"`
	Files            []RecoveryFile `json:"files"`
}
type RecoveryFile struct {
	Disposable bool   `json:"disposable"`
	State      string `json:"state,omitempty"`
	ID         string `json:"id"`
	Path       string `json:"path"`
	Side       string `json:"side"`
}

// recoveryBinding authenticates before revealing any saved paths. Recovery of
// an archived remote file does not require the mapped local directory to exist.
func (e *Engine) recoveryBinding(ctx context.Context, id string) (SavedBinding, *Remote, error) {
	if e.State == nil || e.Remote == nil {
		return SavedBinding{}, nil, syncproto.ErrInvalid
	}
	saved, err := e.State.Load(id)
	if err != nil {
		return SavedBinding{}, nil, err
	}
	identity, err := e.Remote.Identity(ctx)
	if err != nil {
		return SavedBinding{}, nil, err
	}
	if saved.Binding.Server != e.Remote.base.String() || saved.Binding.User != identity.User {
		return SavedBinding{}, nil, ErrBinding
	}
	// Local history belongs to this OS user's saved URL/user. A replacement
	// instance may inspect metadata/local copies, but never old remote bytes.
	var remote *Remote
	if identity.ServerID == saved.Binding.ServerID {
		pinned := *e.Remote
		pinned.serverID = identity.ServerID
		remote = &pinned
	}
	return saved, remote, nil
}
func validateRecoveryBatch(batch Batch, binding syncproto.Binding) error {
	if !syncproto.ValidOperationID(batch.ID) || batch.Plan.Binding != binding || len(batch.Plan.Operations) != len(batch.Operations) || batch.Plan.Ready(batch.Plan.Digest) != nil {
		return syncproto.ErrInvalid
	}
	completed := completedRecoveryBatch(batch)
	for i, op := range batch.Operations {
		plan := batch.Plan.Operations[i]
		if op.RemoteRetirement != "" && (remoteKind(plan.Kind) == "" || !completed || (op.RemoteRetirement != "retiring" && op.RemoteRetirement != "retired")) {
			return syncproto.ErrInvalid
		}
		if op.RecoveryState != "" && (op.Recovery == nil || remoteKind(plan.Kind) != "" || !completed || (op.RecoveryState != "discarding" && op.RecoveryState != "discarded")) {
			return syncproto.ErrInvalid
		}
		if !syncproto.ValidOperationID(op.ID) || !syncproto.ValidPath(plan.Path) {
			return syncproto.ErrInvalid
		}
		if op.Status != "prepared" && op.Status != "started" && op.Status != "verified" {
			return syncproto.ErrInvalid
		}
		if op.Recovery != nil && (plan.Before == nil || plan.Before.Kind != "file" || op.Recovery.Hash != plan.Before.Hash || op.Recovery.Size != plan.Before.Size || !recoveryPathValid(op.Recovery.Path)) {
			return syncproto.ErrInvalid
		}
	}
	return nil
}
func recoverySummary(batch Batch, pending bool) RecoveryBatch {
	return recoveryWindow(batch, pending, 0, len(batch.Operations))
}

func recoveryWindow(batch Batch, pending bool, offset, limit int) RecoveryBatch {
	result := RecoveryBatch{ID: batch.ID, Pending: pending, Action: "completed", Files: []RecoveryFile{}, FileOffset: offset, Cleanable: historyCleanable(batch, pending)}
	disposable := !pending && completedRecoveryBatch(batch)
	add := func(file RecoveryFile) {
		if result.TotalFiles >= offset && len(result.Files) < limit {
			result.Files = append(result.Files, file)
		}
		result.TotalFiles++
	}
	if pending {
		result.Action = "pending"
	} else if batch.Resolution != nil {
		result.Action = batch.Resolution.Action
	}
	for i, op := range batch.Operations {
		plan := batch.Plan.Operations[i]
		if remoteKind(plan.Kind) != "" {
			result.RemoteOperations++
			if op.RemoteRetirement == "retired" {
				result.RemoteRetired++
			}
		}
		if op.Recovery != nil {
			add(RecoveryFile{ID: op.ID, Path: plan.Path, Side: "local", State: op.RecoveryState, Disposable: disposable && op.RecoveryState != "discarded"})
		} else if remoteKind(plan.Kind) != "" && op.Status != "prepared" && plan.Before != nil && plan.Before.Kind == "file" {
			// Receipt/download will verify actual availability. A started request might
			// never have reached the server; do not claim every candidate has old bytes.
			add(RecoveryFile{ID: op.ID, Path: plan.Path, Side: "remote", State: op.RemoteRetirement})
		}
	}
	result.RemoteCleanable = disposable && result.RemoteOperations > result.RemoteRetired
	return result
}
func (e *Engine) RecoveryHistory(ctx context.Context, id string) ([]RecoveryBatch, error) {
	saved, _, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = e.State.root.CheckIdentity(); err != nil {
		return nil, err
	}
	progressStage(ctx, "history")
	rows, err := e.State.db.QueryContext(ctx, "SELECT id FROM batches WHERE binding=? ORDER BY rowid DESC", id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, value)
		if len(ids) > 1000 {
			rows.Close()
			return nil, syncproto.ErrLimit
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(ids) > 1000 {
		return nil, syncproto.ErrLimit
	}
	result := []RecoveryBatch{}
	if saved.Pending != nil {
		if err = validateRecoveryBatch(*saved.Pending, saved.Binding); err != nil {
			return nil, err
		}
		result = append(result, recoverySummary(*saved.Pending, true))
	}
	for _, batchID := range ids {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := e.State.History(id, batchID)
		if err != nil {
			return nil, err
		}
		if err = validateRecoveryBatch(batch, saved.Binding); err != nil {
			return nil, err
		}
		result = append(result, recoverySummary(batch, false))
	}
	return result, nil
}

// ExportRecovery always creates one new file in a native-selected external
// directory. It never restores over a mapped file, follows links or accepts an
// arbitrary source path from the renderer. The old bytes are verified before
// no-replace publication; failure leaves existing destination files untouched.
func (e *Engine) ExportRecovery(ctx context.Context, id, batchID, operationID, directory string) (string, error) {
	if !syncproto.ValidOperationID(batchID) || !syncproto.ValidOperationID(operationID) {
		return "", syncproto.ErrInvalid
	}
	saved, remote, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return "", err
	}
	var batch Batch
	if saved.Pending != nil && saved.Pending.ID == batchID {
		batch = *saved.Pending
	} else {
		batch, err = e.State.History(id, batchID)
		if err != nil {
			return "", err
		}
	}
	if err = validateRecoveryBatch(batch, saved.Binding); err != nil {
		return "", err
	}
	index := -1
	for i, op := range batch.Operations {
		if op.ID == operationID {
			index = i
			break
		}
	}
	if index < 0 {
		return "", syncproto.ErrInvalid
	}
	op, plan := batch.Operations[index], batch.Plan.Operations[index]
	if op.RecoveryState == "discarded" || op.RemoteRetirement != "" {
		return "", syncproto.ErrInvalid
	}
	if plan.Before == nil || plan.Before.Kind != "file" || op.Status == "prepared" {
		return "", syncproto.ErrInvalid
	}
	var source io.ReadCloser
	if remoteKind(plan.Kind) != "" {
		if remote == nil {
			return "", ErrBinding
		}
		status, err := remote.Operation(ctx, saved.Binding.Workspace, operationID)
		if err != nil {
			return "", err
		}
		if !matchesReceipt(status, plan) || !status.Operation.Recovery || status.Retirement != "" {
			return "", ErrProtocol
		}
		source, err = remote.Recovery(ctx, saved.Binding.Workspace, status)
		if err != nil {
			return "", err
		}
	} else {
		if op.Recovery == nil || !recoveryPathValid(op.Recovery.Path) {
			return "", syncproto.ErrInvalid
		}
		root, err := syncfs.OpenContext(ctx, saved.Directory)
		if err != nil {
			return "", err
		}
		defer root.Close()
		identity, err := root.Identity()
		if err != nil {
			return "", err
		}
		if identity != saved.Binding.LocalID {
			return "", ErrBinding
		}
		source, err = root.OpenFile(op.Recovery.Path)
		if err != nil {
			return "", err
		}
	}
	defer source.Close()
	target, err := e.State.exportDestination(ctx, directory)
	if err != nil {
		return "", err
	}
	defer target.Close()
	filename := "agentbox-recovery-" + operationID + ".bak"
	expected := *plan.Before
	expected.Executable = false
	progressPlan(ctx, []Operation{{Kind: "download", After: &expected}})
	progressOperation(ctx, Operation{Kind: "download", Path: plan.Path, After: &expected})
	changeProgress(ctx, func(p *progressState) { p.value.Stage = "exporting" })
	writer := syncfs.Writer{Root: target}
	if _, err = writer.Replace(ctx, filename, nil, expected, trackProgress(ctx, source)); err != nil {
		return "", err
	}
	progressVerified(ctx)
	return filename, nil
}

func (s *StateStore) exportDestination(ctx context.Context, directory string) (*syncfs.Root, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if pathsOverlap(absolute, s.directory) {
		return nil, ErrBinding
	}
	target, err := syncfs.OpenContext(ctx, absolute)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*syncfs.Root, error) { target.Close(); return nil, err }
	identity, err := target.Identity()
	if err != nil {
		return fail(err)
	}
	ancestors, err := target.AncestorIdentitiesContext(ctx)
	if err != nil {
		return fail(err)
	}
	stateAncestors, err := s.root.AncestorIdentitiesContext(ctx)
	if err != nil {
		return fail(err)
	}
	if identity != ancestors[0] || slices.Contains(ancestors, stateAncestors[0]) || slices.Contains(stateAncestors, identity) {
		return fail(ErrBinding)
	}
	if err = s.root.CheckIdentity(); err != nil {
		return fail(err)
	}
	rows, err := s.db.Query("SELECT state FROM bindings")
	if err != nil {
		return fail(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var saved SavedBinding
		if err = rows.Scan(&raw); err != nil {
			return fail(err)
		}
		if err = decodeState(raw, &saved); err != nil {
			return fail(err)
		}
		if pathsOverlap(absolute, saved.Directory) || slices.Contains(ancestors, saved.Binding.LocalID) || slices.Contains(saved.Ancestors, identity) {
			return fail(ErrBinding)
		}
	}
	if err = rows.Err(); err != nil {
		return fail(err)
	}
	return target, nil
}
