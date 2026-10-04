package server

import (
	"encoding/json"
	"errors"
	"testing"

	"agentbox/internal/syncclient"
)

func TestSyncHistoryCleanupOnlyRemovesConfirmedSafeMetadata(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "a", "original")
	saved := engineRound(t, f)
	first, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || first.History[0].Cleanable {
		t.Fatal(first, err)
	}
	noCopyID := first.History[0].ID
	if _, err = f.engine.CleanupHistory(t.Context(), saved.ID, noCopyID, first.Revision); err == nil {
		t.Fatal("history removed before remote cleanup")
	}
	review, err := f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, noCopyID, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.CleanupRemoteRecovery(t.Context(), saved.ID, noCopyID, first.Revision, review.Digest); err != nil {
		t.Fatal(err)
	}
	first, err = f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || !first.History[0].Cleanable {
		t.Fatal(first, err)
	}
	writeEngineFile(t, f.local, "a", "new")
	saved = engineRound(t, f)
	page, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || page.History[0].Cleanable {
		t.Fatal("recoverable history marked cleanable", page, err)
	}
	if _, err = f.engine.CleanupHistory(t.Context(), saved.ID, noCopyID, first.Revision); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("stale cleanup accepted", err)
	}
	if _, err = f.engine.CleanupHistory(t.Context(), saved.ID, page.History[0].ID, page.Revision); err == nil {
		t.Fatal("recoverable metadata removed")
	}
	baseline, _ := json.Marshal(saved.Baseline)
	archived, err := f.engine.ArchiveBinding(t.Context(), saved.ID, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	cleaned, err := f.engine.CleanupHistory(t.Context(), saved.ID, noCopyID, archived.Revision)
	if err != nil || !cleaned.Archived || cleaned.Revision != archived.Revision+1 {
		t.Fatal(cleaned, err)
	}
	now, _ := json.Marshal(cleaned.Baseline)
	if string(now) != string(baseline) {
		t.Fatal("cleanup rewrote baseline")
	}
	final, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || len(final.History) != 1 || final.History[0].ID != page.History[0].ID || final.Metadata.Bytes >= page.Metadata.Bytes {
		t.Fatal("wrong history/capacity after cleanup", final, err)
	}
	if _, err = f.engine.CleanupHistory(t.Context(), saved.ID, noCopyID, archived.Revision); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("cleanup replay accepted", err)
	}
}

func TestSyncHistoryCleanupAndAbandonRejectWrongUser(t *testing.T) {
	f, _ := lostEngineResponse(t, map[string]string{"a": "pending"})
	remote, err := syncclient.NewRemote(f.saved.Binding.Server, "client-fixture-bob", false)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	other := syncclient.Engine{State: f.engine.State, Remote: remote}
	if _, err = other.ReviewAbandon(t.Context(), f.saved.ID); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("foreign review", err)
	}
	if _, err = other.CleanupHistory(t.Context(), f.saved.ID, f.saved.ID, f.saved.Revision); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("foreign cleanup", err)
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.CleanupHistory(t.Context(), saved.ID, saved.Pending.ID, saved.Revision); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("pending cleanup", err)
	}
}
