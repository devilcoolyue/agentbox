package syncclient

import (
	"context"
	"encoding/json"

	"agentbox/internal/syncproto"
)

// AbandonReview describes saved intent only. It makes no assertion about file
// contents, applied writes or the availability of an old recovery copy.
type AbandonReview struct {
	BindingID     string `json:"binding_id"`
	BatchID       string `json:"batch_id"`
	Revision      int64  `json:"revision"`
	ServerID      string `json:"server_id"`
	ServerChanged bool   `json:"server_changed"`
	Prepared      int    `json:"prepared"`
	Started       int    `json:"started"`
	Verified      int    `json:"verified"`
	StateDigest   string `json:"state_digest"`
	Digest        string `json:"digest"`
}

func (e *Engine) abandonReview(ctx context.Context, id string) (AbandonReview, SavedBinding, *Remote, error) {
	saved, remote, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return AbandonReview{}, SavedBinding{}, nil, err
	}
	if saved.Archived || saved.Pending == nil {
		return AbandonReview{}, SavedBinding{}, nil, ErrStateChanged
	}
	// Include the authenticated replacement identity in the confirmation without
	// sending any old operation IDs to it.
	identity, err := e.Remote.Identity(ctx)
	if err != nil {
		return AbandonReview{}, SavedBinding{}, nil, err
	}
	if identity.User != saved.Binding.User || (identity.ServerID == saved.Binding.ServerID) != (remote != nil) {
		return AbandonReview{}, SavedBinding{}, nil, ErrBinding
	}
	report := AbandonReview{BindingID: id, BatchID: saved.Pending.ID, Revision: saved.Revision, ServerID: identity.ServerID, ServerChanged: remote == nil}
	for _, op := range saved.Pending.Operations {
		switch op.Status {
		case "prepared":
			report.Prepared++
		case "started":
			report.Started++
		case "verified":
			report.Verified++
		}
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return AbandonReview{}, SavedBinding{}, nil, err
	}
	report.StateDigest = syncproto.HashBytes(raw)
	raw, err = json.Marshal(report)
	if err != nil {
		return AbandonReview{}, SavedBinding{}, nil, err
	}
	report.Digest = syncproto.HashBytes(raw)
	return report, saved, remote, nil
}

func (e *Engine) ReviewAbandon(ctx context.Context, id string) (AbandonReview, error) {
	report, _, _, err := e.abandonReview(ctx, id)
	return report, err
}

// AbandonPending retires the entire mapping, never just the pending flag. The
// original baseline and uncertain operations remain available as history, and
// a new mapping must start without any baseline. With the original server we
// acquire a fresh lease to exclude an active executor, even if the local root
// is lost. A replacement instance never receives old project/operation IDs.
func (e *Engine) AbandonPending(ctx context.Context, id, confirmation string) (SavedBinding, error) {
	report, saved, remote, err := e.abandonReview(ctx, id)
	if err != nil {
		return SavedBinding{}, err
	}
	if !syncproto.ValidHash(confirmation) || report.Digest != confirmation {
		return SavedBinding{}, ErrStateChanged
	}
	guard := func() error { return ctx.Err() }
	if remote != nil {
		device, err := e.State.Device()
		if err != nil {
			return SavedBinding{}, err
		}
		lease, err := startLease(ctx, remote, saved.Binding, device)
		if err != nil {
			return SavedBinding{}, err
		}
		defer lease.Close()
		guard = func() error { return lease.Guard(ctx) }
	}
	return e.State.change(id, report.Revision, func(current *SavedBinding) error {
		if err := guard(); err != nil {
			return err
		}
		if current.Pending == nil || current.Pending.ID != report.BatchID {
			return ErrStateChanged
		}
		current.Pending.Resolution = &BatchResolution{Action: "abandoned", ReviewDigest: report.Digest}
		current.Pending = nil
		current.Archived = true
		return nil
	}, true)
}
