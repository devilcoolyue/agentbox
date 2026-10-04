package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentbox/internal/syncproto"
)

// PendingReview describes observations, not proof that a past write happened.
// Applied receipts are historical; current file states and complete manifests
// are checked independently. No file is changed by reviewing or resolving.
type PendingReview struct {
	Version         int          `json:"version"`
	BindingID       string       `json:"binding_id"`
	BatchID         string       `json:"batch_id"`
	StateRevision   int64        `json:"state_revision"`
	ProjectRevision int64        `json:"project_revision"`
	LocalDigest     string       `json:"local_digest"`
	RemoteDigest    string       `json:"remote_digest"`
	RulesChanged    bool         `json:"rules_changed"`
	FinalMatches    bool         `json:"final_matches"`
	CanFinish       bool         `json:"can_finish"`
	Items           []ReviewItem `json:"items"`
	Digest          string       `json:"digest"`
}
type ReviewItem struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Recorded string `json:"recorded"`
	Current  string `json:"current"` // before, after, diverged; this is target-side state.
	Receipt  string `json:"receipt"` // local, not_attempted, missing, applied, uncertain.
	Recovery bool   `json:"recovery"`
}
type BatchResolution struct {
	Action       string `json:"action"` // finish, replan, or abandoned (no file observations)
	ReviewDigest string `json:"review_digest"`
	LocalDigest  string `json:"local_digest"`
	RemoteDigest string `json:"remote_digest"`
}

func remoteKind(kind string) string {
	switch kind {
	case "upload":
		return "replace"
	case "delete_remote":
		return "delete"
	case "mkdir_remote":
		return "mkdir"
	case "rmdir_remote":
		return "rmdir"
	}
	return ""
}
func matchesReceipt(status syncproto.OperationStatus, operation Operation) bool {
	return status.Path == operation.Path && status.Kind == remoteKind(operation.Kind) && equivalent(status.Before, operation.Before, true) && equivalent(status.After, operation.After, true)
}

func (e *Engine) reviewOpen(ctx context.Context, id string) (openedBinding, *leaseKeeper, error) {
	o, err := e.openBinding(ctx, id, true, true)
	if err != nil {
		return o, nil, err
	}
	if o.saved.Pending == nil {
		o.root.Close()
		return openedBinding{}, nil, ErrStateChanged
	}
	device, err := e.State.Device()
	if err != nil {
		o.root.Close()
		return openedBinding{}, nil, err
	}
	progressStage(ctx, "lease")
	lease, err := startLease(ctx, o.remote, o.saved.Binding, device)
	if err != nil {
		o.root.Close()
		return openedBinding{}, nil, err
	}
	return o, lease, nil
}

// ReviewPending reacquires the project lease so an active executor cannot be
// silently dismissed. It does not renew an old generation or replay mutations.
func (e *Engine) ReviewPending(ctx context.Context, id string) (PendingReview, error) {
	o, lease, err := e.reviewOpen(ctx, id)
	if err != nil {
		return PendingReview{}, err
	}
	defer o.root.Close()
	defer lease.Close()
	report, _, _, err := e.inspectPending(lease.ctx, o, lease)
	return report, err
}
func (e *Engine) inspectPending(ctx context.Context, o openedBinding, lease *leaseKeeper) (PendingReview, syncproto.Manifest, syncproto.Manifest, error) {
	fail := func(err error) (PendingReview, syncproto.Manifest, syncproto.Manifest, error) {
		return PendingReview{}, syncproto.Manifest{}, syncproto.Manifest{}, err
	}
	local, remote, err := o.scan(ctx)
	if err != nil {
		return fail(err)
	}
	batch := o.saved.Pending
	report := PendingReview{Version: 1, BindingID: o.saved.ID, BatchID: batch.ID, StateRevision: o.saved.Revision, ProjectRevision: remote.Revision, Items: []ReviewItem{}}
	report.LocalDigest, _ = local.Digest()
	report.RemoteDigest = remote.Digest
	report.RulesChanged = local.RulesHash != batch.Local.RulesHash || remote.Manifest.RulesHash != batch.Remote.RulesHash
	expectedLocal, expectedRemote, err := projected(batch)
	if err != nil {
		return fail(err)
	}
	report.FinalMatches = sameManifest(expectedLocal, local) && sameManifest(expectedRemote, remote.Manifest)
	report.CanFinish = report.FinalMatches && remote.Revision == batch.ProjectRevision && o.caps.Executable == batch.Options.LocalExecutable
	progressStage(ctx, "reviewing")
	for i, operation := range batch.Plan.Operations {
		recorded := batch.Operations[i]
		item := ReviewItem{ID: recorded.ID, Path: operation.Path, Kind: operation.Kind, Recorded: recorded.Status, Receipt: "local", Current: "diverged", Recovery: recorded.Recovery != nil}
		target, executable := local, o.caps.Executable
		if remoteKind(operation.Kind) != "" {
			target, executable = remote.Manifest, true
			item.Receipt = "not_attempted"
			if recorded.Status != "prepared" {
				status, err := o.remote.Operation(ctx, o.saved.Binding.Workspace, recorded.ID)
				var rejected *HTTPError
				if errors.As(err, &rejected) && rejected.Status == http.StatusNotFound {
					item.Receipt = "missing"
				} else if err != nil {
					return fail(err)
				} else {
					if !matchesReceipt(status, operation) {
						return fail(ErrProtocol)
					}
					item.Receipt = status.Operation.Status
					if status.Retirement != "" {
						item.Receipt = status.Retirement
					}
					item.Recovery = status.Operation.Recovery
				}
			}
			if item.Receipt != "applied" {
				report.CanFinish = false
			}
		}
		actual := entry(target, operation.Path)
		if equivalent(actual, operation.After, executable) {
			item.Current = "after"
		} else if equivalent(actual, operation.Before, executable) {
			item.Current = "before"
		}
		if recorded.Status == "prepared" {
			report.CanFinish = false
		}
		report.Items = append(report.Items, item)
	}
	// A receipt query can take time. Require scans unchanged across the review;
	// never label an observation complete using a stale initial manifest.
	againLocal, againRemote, err := o.scan(ctx)
	if err != nil {
		return fail(err)
	}
	if !sameManifest(local, againLocal) || againRemote.Digest != remote.Digest || againRemote.Revision != remote.Revision {
		return fail(syncproto.ErrChanged)
	}
	if err = lease.Guard(ctx); err != nil {
		return fail(err)
	}
	current, err := e.State.Load(o.saved.ID)
	if err != nil {
		return fail(err)
	}
	if current.Revision != o.saved.Revision {
		return fail(ErrStateChanged)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fail(err)
	}
	report.Digest = syncproto.HashBytes(encoded)
	return report, local, remote.Manifest, nil
}

// ResolvePending requires the exact current review digest. finish only commits
// when the full intended result exists and all remote writes have applied
// receipts. replan archives without changing files or advancing the old baseline.
func (e *Engine) ResolvePending(ctx context.Context, id, confirmation, action string) (SavedBinding, error) {
	if !syncproto.ValidHash(confirmation) || (action != "finish" && action != "replan") {
		return SavedBinding{}, syncproto.ErrInvalid
	}
	o, lease, err := e.reviewOpen(ctx, id)
	if err != nil {
		return SavedBinding{}, err
	}
	defer o.root.Close()
	defer lease.Close()
	report, local, remote, err := e.inspectPending(lease.ctx, o, lease)
	if err != nil {
		return SavedBinding{}, err
	}
	if report.Digest != confirmation {
		return SavedBinding{}, ErrStateChanged
	}
	if action == "finish" && !report.CanFinish {
		return SavedBinding{}, ErrPending
	}
	if err = context.Cause(lease.ctx); err != nil {
		return SavedBinding{}, err
	}
	progressStage(lease.ctx, "committing")
	return e.State.resolvePending(id, o.saved.Revision, report, action, local, remote)
}
func (s *StateStore) resolvePending(id string, revision int64, report PendingReview, action string, local, remote syncproto.Manifest) (SavedBinding, error) {
	return s.change(id, revision, func(saved *SavedBinding) error {
		batch := saved.Pending
		if batch == nil || batch.ID != report.BatchID || report.BindingID != id || report.StateRevision != revision || !syncproto.ValidHash(report.Digest) {
			return ErrStateChanged
		}
		localDigest, err := local.Digest()
		if err != nil {
			return err
		}
		remoteDigest, err := remote.Digest()
		if err != nil {
			return err
		}
		if localDigest != report.LocalDigest || remoteDigest != report.RemoteDigest {
			return ErrStateChanged
		}
		if action == "finish" {
			if !report.CanFinish || len(report.Items) != len(batch.Operations) {
				return ErrPending
			}
			for i, op := range batch.Operations {
				if op.Status == "prepared" || report.Items[i].ID != op.ID || remoteKind(batch.Plan.Operations[i].Kind) != "" && report.Items[i].Receipt != "applied" {
					return ErrPending
				}
			}
			wantLocal, wantRemote, err := projected(batch)
			if err != nil {
				return err
			}
			if !sameManifest(local, wantLocal) || !sameManifest(remote, wantRemote) {
				return syncproto.ErrChanged
			}
			saved.Baseline = &syncproto.Baseline{Binding: saved.Binding, Local: local, Remote: remote}
		} else if action != "replan" {
			return syncproto.ErrInvalid
		}
		batch.Resolution = &BatchResolution{Action: action, ReviewDigest: report.Digest, LocalDigest: localDigest, RemoteDigest: remoteDigest}
		saved.Pending = nil
		return nil
	}, true)
}

// RecoveryPathValid accepts only files created by the project-wide recovery
// writer. A corrupted journal cannot turn export into arbitrary local reads.
func recoveryPathValid(path string) bool {
	return syncproto.ValidPath(path) && strings.HasPrefix(path, ".agentbox-sync/recovery/") && strings.Count(path, "/") == 2 && strings.HasSuffix(path, ".bak")
}
