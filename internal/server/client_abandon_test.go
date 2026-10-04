package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/syncclient"
	"agentbox/internal/syncproto"
)

func TestSyncAbandonMissingRootRetiresMappingAndKeepsUnknownBatch(t *testing.T) {
	f, _ := lostEngineResponse(t, map[string]string{"file": "published"})
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := f.engine.ReviewAbandon(t.Context(), saved.ID)
	if err != nil || report.Started != 1 || report.ServerChanged {
		t.Fatal(report, err)
	}
	if _, err = f.engine.AbandonPending(t.Context(), saved.ID, strings.Repeat("0", 64)); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("stale confirmation accepted", err)
	}
	lease, err := f.engine.Remote.Acquire(t.Context(), saved.Binding.Workspace, saved.Binding.Project, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.AbandonPending(t.Context(), saved.ID, report.Digest); err == nil {
		t.Fatal("active lease bypassed")
	}
	if err = f.engine.Remote.Release(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	moved := f.local + "-moved"
	if err = os.Rename(f.local, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(moved, f.local) })
	archived, err := f.engine.AbandonPending(t.Context(), saved.ID, report.Digest)
	if err != nil || !archived.Archived || archived.Pending != nil {
		t.Fatal(archived, err)
	}
	if saved.Baseline != nil || archived.Baseline != nil {
		t.Fatal("abandonment created baseline")
	}
	history, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || history.History[0].Action != "abandoned" || history.LocalRecovery != nil || history.LocalRecoveryStatus != "unavailable" {
		t.Fatal(history, err)
	}
	batch, err := f.engine.State.History(saved.ID, saved.Pending.ID)
	if err != nil || batch.Resolution.Action != "abandoned" || batch.Resolution.LocalDigest != "" || batch.Resolution.RemoteDigest != "" || batch.Operations[0].Status != "started" {
		t.Fatal("unknown result converted to successful history", batch, err)
	}
	for _, root := range []string{moved, f.remote} {
		raw, err := os.ReadFile(filepath.Join(root, "file"))
		if err != nil || string(raw) != "published" {
			t.Fatal("abandonment changed files", err)
		}
	}
	if _, err = f.engine.AbandonPending(t.Context(), saved.ID, report.Digest); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("abandonment replayed", err)
	}
}

func TestSyncAbandonReplacementNeverSendsOldReferences(t *testing.T) {
	var replacement atomic.Bool
	var requests atomic.Int32
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if replacement.Load() {
				if r.URL.Path != "/api/clients/capabilities" {
					requests.Add(1)
					http.Error(w, "old reference", 500)
					return
				}
				rr := httptest.NewRecorder()
				next.ServeHTTP(rr, r)
				var identity syncproto.ServerIdentity
				if json.Unmarshal(rr.Body.Bytes(), &identity) != nil {
					t.Error("identity")
					w.WriteHeader(500)
					return
				}
				identity.ServerID = syncproto.HashBytes([]byte("new instance"))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(identity)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "a", "local")
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	// Persist a prepared batch without any file mutation, as after a crash.
	rootManifest, err := f.engine.Remote.Manifest(t.Context(), f.saved.Binding.Workspace, f.saved.Binding.Project)
	if err != nil {
		t.Fatal(err)
	}
	localManifest := rootManifest.Manifest
	localManifest.Entries = map[string]syncproto.Entry{"a": {Kind: "file", Hash: syncproto.HashBytes([]byte("local")), Size: 5}}
	saved, err := f.engine.State.Begin(f.saved.ID, f.saved.Revision, preview.ProjectRevision, preview.Plan, preview.Plan.Digest, localManifest, rootManifest.Manifest, preview.Options)
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.engine.ReviewAbandon(t.Context(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Store(true)
	if _, err = f.engine.AbandonPending(t.Context(), saved.ID, old.Digest); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("identity change accepted old confirmation", err)
	}
	report, err := f.engine.ReviewAbandon(t.Context(), saved.ID)
	if err != nil || !report.ServerChanged {
		t.Fatal(report, err)
	}
	if _, err = f.engine.AbandonPending(t.Context(), saved.ID, report.Digest); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("old references sent to replacement")
	}
	newBinding := saved.Binding
	newBinding.ServerID = syncproto.HashBytes([]byte("new instance"))
	fresh, err := f.engine.State.Register(newBinding, saved.Directory)
	if err != nil || fresh.ID == saved.ID || fresh.Baseline != nil {
		t.Fatal("retired baseline reused", err)
	}
}
