package syncclient

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

// OrphanRecoveryScope is supplied by the authenticated native application. The
// server ID is the one the user reviewed, never silently the next instance at
// the same URL. Device selects an original journal label; a newly initialized
// local database need not pretend to own that device identity.
type OrphanRecoveryScope struct{ ServerID, User, Workspace, Device, OperationID string }

func (e *Engine) orphanRemote(ctx context.Context, scope OrphanRecoveryScope, operation, retire bool) (*Remote, error) {
	if e.Remote == nil || !syncproto.ValidHash(scope.ServerID) || scope.User == "" || !syncID(scope.Workspace) || scope.Device != "" && !syncID(scope.Device) || operation && (!syncproto.ValidOperationID(scope.OperationID) || !syncID(scope.Device)) {
		return nil, syncproto.ErrInvalid
	}
	identity, err := e.Remote.Identity(ctx)
	if err != nil {
		return nil, err
	}
	if identity.ServerID != scope.ServerID || identity.User != scope.User {
		return nil, ErrBinding
	}
	if identity.Features["sync_recovery_inspect"] != 1 || retire && identity.Features["sync_recovery_gc"] != 1 {
		return nil, ErrSyncUnavailable
	}
	pinned := *e.Remote
	pinned.serverID = scope.ServerID
	return &pinned, nil
}

func (e *Engine) ListOrphanRecovery(ctx context.Context, scope OrphanRecoveryScope, cursor string) (syncproto.RecoveryOperationsPage, error) {
	var page syncproto.RecoveryOperationsPage
	remote, err := e.orphanRemote(ctx, scope, false, false)
	if err != nil {
		return page, err
	}
	if len(cursor) > 4096 {
		return page, syncproto.ErrInvalid
	}
	response, err := remote.request(ctx, http.MethodGet, scope.Workspace, "recovery-operations", url.Values{"device": {scope.Device}, "cursor": {cursor}}, nil, "")
	if err != nil {
		return page, err
	}
	if err = decodeRemote(response, 1<<20, &page); err != nil {
		return page, err
	}
	if page.Validate(scope.ServerID, scope.Workspace, scope.Device) != nil {
		return syncproto.RecoveryOperationsPage{}, ErrProtocol
	}
	return page, nil
}

func inspectOrphanRecovery(ctx context.Context, remote *Remote, scope OrphanRecoveryScope) (syncproto.RecoveryOperationReview, error) {
	var review syncproto.RecoveryOperationReview
	response, err := remote.request(ctx, http.MethodGet, scope.Workspace, "recovery-operations/"+scope.OperationID, url.Values{"device": {scope.Device}}, nil, "")
	if err != nil {
		return review, err
	}
	if err = decodeRemote(response, 32<<10, &review); err != nil {
		return review, err
	}
	if review.Validate(scope.ServerID, scope.Workspace, scope.Device, scope.OperationID) != nil {
		return syncproto.RecoveryOperationReview{}, ErrProtocol
	}
	return review, nil
}

func (e *Engine) ReviewOrphanRecovery(ctx context.Context, scope OrphanRecoveryScope) (syncproto.RecoveryOperationReview, error) {
	remote, err := e.orphanRemote(ctx, scope, true, false)
	if err != nil {
		return syncproto.RecoveryOperationReview{}, err
	}
	review, err := inspectOrphanRecovery(ctx, remote, scope)
	if err != nil {
		return review, err
	}
	if e.State == nil {
		return syncproto.RecoveryOperationReview{}, syncproto.ErrInvalid
	}
	tx, err := e.State.db.BeginTx(ctx, nil)
	if err != nil {
		return review, err
	}
	defer tx.Rollback()
	review.LocalPending, err = orphanPendingReference(ctx, tx, scope)
	if err != nil {
		return syncproto.RecoveryOperationReview{}, err
	}
	if review.LocalPending {
		review.CanRetire = false
	}
	return review, nil
}

func orphanPendingReference(ctx context.Context, tx *sql.Tx, scope OrphanRecoveryScope) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT state FROM bindings")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var saved SavedBinding
		if err = rows.Scan(&raw); err != nil {
			return false, err
		}
		if err = decodeState(raw, &saved); err != nil {
			return false, err
		}
		// The instance identity is the boundary. A URL alias or HTTP→HTTPS move
		// must not make a pending reference to the same remote receipt disappear.
		if saved.Binding.ServerID != scope.ServerID || saved.Binding.User != scope.User || saved.Binding.Workspace != scope.Workspace || saved.Pending == nil {
			continue
		}
		for _, operation := range saved.Pending.Operations {
			if operation.ID == scope.OperationID {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func (e *Engine) RetireOrphanRecovery(ctx context.Context, scope OrphanRecoveryScope, confirmation string) (syncproto.RecoveryOperationReview, error) {
	var review syncproto.RecoveryOperationReview
	remote, err := e.orphanRemote(ctx, scope, true, true)
	if err != nil {
		return review, err
	}
	if e.State == nil || !syncproto.ValidHash(confirmation) {
		return review, syncproto.ErrInvalid
	}
	if err = e.State.root.CheckIdentity(); err != nil {
		return review, err
	}
	// BEGIN IMMEDIATE prevents another sidecar from creating a pending reference
	// while this explicit disposal is in flight. Consent is durably stored in the
	// server journal before unlink; no local database/history is needed to resume.
	tx, err := e.State.db.BeginTx(ctx, nil)
	if err != nil {
		return review, err
	}
	defer tx.Rollback()
	pending, err := orphanPendingReference(ctx, tx, scope)
	if err != nil {
		return review, err
	}
	if pending {
		return review, ErrPending
	}
	raw, _ := json.Marshal(syncproto.RecoveryInspectRetireRequest{Device: scope.Device, Confirmation: confirmation})
	response, err := remote.request(ctx, http.MethodPost, scope.Workspace, "recovery-operations/"+scope.OperationID+"/retire", nil, raw, "")
	if err != nil {
		return review, err
	}
	if err = decodeRemote(response, 32<<10, &review); err != nil {
		return review, err
	}
	if review.Validate(scope.ServerID, scope.Workspace, scope.Device, scope.OperationID) != nil || review.Status.Retirement != "retired" || review.Status.Operation.Status != "applied" {
		return syncproto.RecoveryOperationReview{}, ErrProtocol
	}
	if err = e.State.root.CheckIdentity(); err != nil {
		return syncproto.RecoveryOperationReview{}, err
	}
	return review, nil
}

func (e *Engine) ExportOrphanRecovery(ctx context.Context, scope OrphanRecoveryScope, directory string) (string, error) {
	remote, err := e.orphanRemote(ctx, scope, true, false)
	if err != nil {
		return "", err
	}
	review, err := inspectOrphanRecovery(ctx, remote, scope)
	if err != nil {
		return "", err
	}
	if review.RecoveryState != "available" || review.Status.Before == nil || e.State == nil {
		return "", syncproto.ErrInvalid
	}
	// An uncertain receipt can have durable verified before bytes even if the
	// process died before saving its recovery flag. This read does not change the
	// receipt or infer that the attempted mutation ever completed.
	response, err := remote.request(ctx, http.MethodGet, scope.Workspace, "operations/"+scope.OperationID+"/before", nil, nil, "")
	if err != nil {
		return "", err
	}
	source, err := verifiedResponse(ctx, response, *review.Status.Before)
	if err != nil {
		return "", err
	}
	defer source.Close()
	target, err := e.State.exportDestination(ctx, directory)
	if err != nil {
		return "", err
	}
	defer target.Close()
	filename := "agentbox-recovery-" + scope.OperationID + ".bak"
	expected := *review.Status.Before
	expected.Executable = false
	progressPlan(ctx, []Operation{{Kind: "download", After: &expected}})
	progressOperation(ctx, Operation{Kind: "download", Path: review.Status.Path, After: &expected})
	changeProgress(ctx, func(p *progressState) { p.value.Stage = "exporting" })
	writer := syncfs.Writer{Root: target}
	if _, err = writer.Replace(ctx, filename, nil, expected, trackProgress(ctx, source)); err != nil {
		return "", err
	}
	progressVerified(ctx)
	return filename, nil
}
