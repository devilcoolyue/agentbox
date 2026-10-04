package syncclient

import (
	"context"
	"errors"
	"io"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

var ErrSyncUnavailable = errors.New("server has not enabled this sync protocol")

// Engine executes one explicitly previewed batch. It has no background watcher,
// retry policy, UI or credential persistence. The sidecar must own its lifetime
// and cancel calls on logout/parent EOF. sync=0 always refuses Apply.
type Engine struct {
	State  *StateStore
	Remote *Remote
}
type Preview struct {
	BaselineCurrent bool        `json:"baseline_current"`
	Plan            Plan        `json:"plan"`
	StateRevision   int64       `json:"state_revision"`
	ProjectRevision int64       `json:"project_revision"`
	Options         PlanOptions `json:"options"`
}
type openedBinding struct {
	saved  SavedBinding
	root   *syncfs.Root
	caps   syncfs.Capabilities
	remote *Remote
}

func (e *Engine) open(ctx context.Context, id string, write bool) (openedBinding, error) {
	return e.openBinding(ctx, id, write, false)
}
func (e *Engine) openBinding(ctx context.Context, id string, write, pending bool) (openedBinding, error) {
	var opened openedBinding
	if e.State == nil || e.Remote == nil {
		return opened, syncproto.ErrInvalid
	}
	saved, err := e.State.Load(id)
	if err != nil {
		return opened, err
	}
	if saved.Archived {
		return opened, ErrBinding
	}
	if saved.Pending != nil && !pending {
		return opened, ErrPending
	}
	identity, err := e.Remote.Identity(ctx)
	if err != nil {
		return opened, err
	}
	if identity.ServerID != saved.Binding.ServerID || identity.User != saved.Binding.User || e.Remote.base.String() != saved.Binding.Server {
		return opened, ErrBinding
	}
	if write && identity.Features["sync"] != syncproto.Version {
		return opened, ErrSyncUnavailable
	}
	pinned := *e.Remote
	pinned.serverID = saved.Binding.ServerID
	root, err := syncfs.OpenContext(ctx, saved.Directory)
	if err != nil {
		return opened, err
	}
	caps, err := root.Probe()
	if err != nil {
		root.Close()
		return opened, err
	}
	if caps.DirectoryID != saved.Binding.LocalID {
		root.Close()
		return opened, ErrBinding
	}
	return openedBinding{saved: saved, root: root, caps: caps, remote: &pinned}, nil
}
func (o openedBinding) scan(ctx context.Context) (syncproto.Manifest, syncproto.ManifestResponse, error) {
	progressStage(ctx, "local_scan")
	local, err := scanLocal(ctx, o.root, o.caps)
	if err != nil {
		return syncproto.Manifest{}, syncproto.ManifestResponse{}, err
	}
	progressStage(ctx, "remote_scan")
	remote, err := o.remote.Manifest(ctx, o.saved.Binding.Workspace, o.saved.Binding.Project)
	if err != nil {
		return syncproto.Manifest{}, syncproto.ManifestResponse{}, err
	}
	if remote.ProjectPath != o.saved.Binding.ProjectPath {
		return syncproto.Manifest{}, syncproto.ManifestResponse{}, ErrBinding
	}
	if err = syncfs.RequireNames(remote.Manifest, o.caps.NamePolicy); err != nil {
		return syncproto.Manifest{}, syncproto.ManifestResponse{}, err
	}
	return local, remote, nil
}
func (e *Engine) Preview(ctx context.Context, id string, direction Direction) (Preview, error) {
	return e.PreviewChoices(ctx, id, direction, nil, "")
}

// PreviewChoices pins the user's per-file decisions to the exact automatic
// preview they inspected. Apply independently scans and verifies again.
func (e *Engine) PreviewChoices(ctx context.Context, id string, direction Direction, choices map[string]Direction, basisDigest string) (Preview, error) {
	o, err := e.open(ctx, id, false)
	if err != nil {
		return Preview{}, err
	}
	defer o.root.Close()
	local, remote, err := o.scan(ctx)
	if err != nil {
		return Preview{}, err
	}
	progressStage(ctx, "planning")
	options := PlanOptions{Direction: direction, LocalExecutable: o.caps.Executable}
	if len(choices) != 0 {
		base, err := BuildPlan(o.saved.Binding, o.saved.Baseline, local, remote.Manifest, options)
		if err != nil {
			return Preview{}, err
		}
		if base.Digest != basisDigest {
			return Preview{}, ErrStateChanged
		}
		options.Choices = choices
	}
	plan, err := BuildPlan(o.saved.Binding, o.saved.Baseline, local, remote.Manifest, options)
	if err != nil {
		return Preview{}, err
	}
	if err = validatePlanNames(plan, local, remote.Manifest, options, o.caps); err != nil {
		return Preview{}, err
	}
	baselineCurrent := o.saved.Baseline != nil && sameManifest(local, o.saved.Baseline.Local) && sameManifest(remote.Manifest, o.saved.Baseline.Remote)
	return Preview{Plan: plan, StateRevision: o.saved.Revision, ProjectRevision: remote.Revision, Options: options, BaselineCurrent: baselineCurrent}, nil
}

// Apply reacquires a lease and rescans both sides before persisting intent. It
// does not accept stale confirmation, silently resume pending work, or commit a
// partial baseline. Failure after Begin leaves durable pending state for review.
func (e *Engine) Apply(ctx context.Context, id string, preview Preview, confirmation string) (SavedBinding, error) {
	o, err := e.open(ctx, id, true)
	if err != nil {
		return SavedBinding{}, err
	}
	defer o.root.Close()
	if o.saved.Revision != preview.StateRevision || o.caps.Executable != preview.Options.LocalExecutable {
		return SavedBinding{}, ErrStateChanged
	}
	if err = preview.Plan.Ready(confirmation); err != nil {
		return SavedBinding{}, err
	}
	device, err := e.State.Device()
	if err != nil {
		return SavedBinding{}, err
	}
	progressStage(ctx, "lease")
	lease, err := startLease(ctx, o.remote, o.saved.Binding, device)
	if err != nil {
		return SavedBinding{}, err
	}
	defer lease.Close()
	work := lease.ctx
	local, remote, err := o.scan(work)
	if err != nil {
		return SavedBinding{}, err
	}
	if remote.Revision != preview.ProjectRevision {
		return SavedBinding{}, ErrStateChanged
	}
	rebuilt, err := BuildPlan(o.saved.Binding, o.saved.Baseline, local, remote.Manifest, preview.Options)
	if err != nil {
		return SavedBinding{}, err
	}
	if rebuilt.Digest != preview.Plan.Digest {
		return SavedBinding{}, ErrStateChanged
	}
	if err = validatePlanNames(rebuilt, local, remote.Manifest, preview.Options, o.caps); err != nil {
		return SavedBinding{}, err
	}
	if err = lease.Guard(work); err != nil {
		return SavedBinding{}, err
	}
	progressPlan(work, rebuilt.Operations)
	saved, err := e.State.Begin(id, o.saved.Revision, remote.Revision, rebuilt, confirmation, local, remote.Manifest, preview.Options)
	if err != nil {
		return SavedBinding{}, err
	}
	writer := syncfs.Writer{Root: o.root, Guard: func(ctx context.Context) error {
		if err := lease.Guard(ctx); err != nil {
			return err
		}
		raw, _, err := readLocalRules(o.root)
		if err != nil {
			return err
		}
		rules, err := syncproto.ParseRules(string(raw))
		if err != nil {
			return err
		}
		if rules.Hash() != local.RulesHash {
			return ErrRulesChanged
		}
		return nil
	}}
	for i, operation := range rebuilt.Operations {
		if err = context.Cause(work); err != nil {
			return saved, err
		}
		progressOperation(work, operation)
		operationID := saved.Pending.Operations[i].ID
		saved, err = e.State.StartOperation(saved.ID, saved.Revision, operationID)
		if err != nil {
			return SavedBinding{}, err
		}
		writer.RecoveryReady = func(recovery syncfs.Recovery) error {
			next, err := e.State.RecordRecovery(saved.ID, saved.Revision, operationID, recovery)
			if err == nil {
				saved = next
			}
			return err
		}
		recovery, err := applyOperation(work, o, lease, &writer, operation, operationID, remote.Revision, local.RulesHash)
		if err != nil {
			return saved, err
		}
		if err = context.Cause(work); err != nil {
			return saved, err
		}
		next, err := e.State.VerifyOperation(saved.ID, saved.Revision, operationID, recovery)
		if err != nil {
			return saved, err
		}
		saved = next
		progressVerified(work)
	}
	// Receipts and successful individual renames are not a project baseline.
	// Full scans detect unrelated edits and source changes during a transfer.
	finalLocal, finalRemote, err := o.scan(work)
	if err != nil {
		return saved, err
	}
	if finalRemote.Revision != preview.ProjectRevision {
		return saved, ErrStateChanged
	}
	if err = lease.Guard(work); err != nil {
		return saved, err
	}
	progressStage(work, "committing")
	next, err := e.State.Commit(saved.ID, saved.Revision, finalLocal, finalRemote.Manifest)
	if err != nil {
		return saved, err
	}
	return next, nil
}

func validatePlanNames(plan Plan, local, remote syncproto.Manifest, options PlanOptions, caps syncfs.Capabilities) error {
	if len(plan.Conflicts) != 0 {
		return nil
	} // Conflicts remain a reviewable preview.
	finalLocal, _, err := projected(&Batch{Plan: plan, Local: local, Remote: remote, Options: options})
	if err != nil {
		return err
	}
	// Separately valid trees can collide when merged (e.g. local Foo, remote foo).
	return syncfs.RequireNames(finalLocal, caps.NamePolicy)
}
func applyOperation(ctx context.Context, o openedBinding, lease *leaseKeeper, writer *syncfs.Writer, operation Operation, id string, revision int64, rulesHash string) (*syncfs.Recovery, error) {
	if err := lease.Guard(ctx); err != nil {
		return nil, err
	}
	switch operation.Kind {
	case "download":
		request := syncproto.FileRequest{Project: o.saved.Binding.Project, Revision: revision, RulesHash: rulesHash, Path: operation.Path, Expected: *operation.After}
		source, err := o.remote.File(ctx, o.saved.Binding.Workspace, request)
		if err != nil {
			return nil, err
		}
		defer source.Close()
		recovery, err := writer.Replace(ctx, operation.Path, operation.Before, *operation.After, trackProgress(ctx, source))
		if recovery.Path == "" {
			return nil, err
		}
		return &recovery, err
	case "delete_local":
		recovery, err := writer.Delete(ctx, operation.Path, *operation.Before)
		if recovery.Path == "" {
			return nil, err
		}
		return &recovery, err
	case "mkdir_local":
		return nil, writer.Mkdir(ctx, operation.Path)
	case "rmdir_local":
		return nil, writer.Rmdir(ctx, operation.Path)
	}
	mutation := syncproto.Mutation{Version: syncproto.Version, ID: id, Project: o.saved.Binding.Project, Revision: revision, RulesHash: rulesHash, Device: lease.grant.Device, Generation: lease.grant.Generation, Path: operation.Path, Before: operation.Before, After: operation.After}
	var source io.Reader
	switch operation.Kind {
	case "upload":
		mutation.Kind = "replace"
		file, err := o.root.OpenFile(operation.Path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		source = trackProgress(ctx, file)
	case "delete_remote":
		mutation.Kind = "delete"
	case "mkdir_remote":
		mutation.Kind = "mkdir"
	case "rmdir_remote":
		mutation.Kind = "rmdir"
	default:
		return nil, syncproto.ErrInvalid
	}
	_, err := o.remote.Apply(ctx, o.saved.Binding.Workspace, lease.grant, mutation, source)
	return nil, err
}
