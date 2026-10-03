package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"agentbox/internal/syncclient"
	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

func TestSyncLifecycleArchiveRetainsRecoveryAndFreshBinding(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "file", "original")
	engineRound(t, f)
	writeEngineFile(t, f.local, "file", "new")
	saved := engineRound(t, f)
	history, err := f.engine.RecoveryHistory(t.Context(), saved.ID)
	if err != nil || len(history) < 1 {
		t.Fatal(err)
	}
	if _, err = f.engine.ArchiveBinding(t.Context(), saved.ID, saved.Revision-1); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("stale archive accepted", err)
	}
	archived, err := f.engine.ArchiveBinding(t.Context(), saved.ID, saved.Revision)
	if err != nil || !archived.Archived || archived.Baseline == nil {
		t.Fatal(archived, err)
	}
	if _, err = f.engine.Preview(t.Context(), saved.ID, syncclient.Automatic); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("archived writer opened", err)
	}
	export, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name, err := f.engine.ExportRecovery(t.Context(), saved.ID, history[0].ID, history[0].Files[0].ID, export)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(export, name))
	if err != nil || string(raw) != "original" {
		t.Fatal("archive lost recovery", err)
	}
	fresh, err := f.engine.State.Register(saved.Binding, saved.Directory)
	if err != nil || fresh.ID == saved.ID || fresh.Baseline != nil {
		t.Fatal("new binding inherited baseline", err)
	}
	writeEngineFile(t, f.local, "file", "third")
	preview, err := f.engine.Preview(t.Context(), fresh.ID, syncclient.Automatic)
	if err != nil || len(preview.Plan.Conflicts) != 1 || preview.Plan.Conflicts[0].Reason != "initial_source_required" {
		t.Fatal("new binding silently trusted old baseline", preview, err)
	}
	if _, err = f.engine.State.Register(saved.Binding, saved.Directory); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("active overlap accepted", err)
	}
	all, err := f.engine.State.BindingsForServer(saved.Binding.Server, "alice")
	if err != nil || len(all) != 2 {
		t.Fatal("archive not discoverable", err)
	}
}

func TestSyncLifecyclePendingCannotArchiveOrBypass(t *testing.T) {
	f, _ := lostEngineResponse(t, map[string]string{"file": "pending"})
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.ArchiveBinding(t.Context(), saved.ID, saved.Revision); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("pending bypassed", err)
	}
	newDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := syncfs.Open(newDir)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := root.Probe()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	replacement := saved.Binding
	replacement.LocalID = caps.DirectoryID
	replacement.ServerID = syncproto.HashBytes([]byte("replaced-server"))
	if _, err = f.engine.State.Register(replacement, newDir); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("new identity bypassed unresolved project", err)
	}
	current, err := f.engine.State.Load(saved.ID)
	if err != nil || current.Pending == nil || current.Archived {
		t.Fatal("pending damaged", err)
	}
}

func TestSyncLifecycleMissingDirectoryCanArchiveWithoutDeletingFiles(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "file", "keep")
	saved := engineRound(t, f)
	moved := f.local + "-moved"
	if err := os.Rename(f.local, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(moved, f.local) })
	archived, err := f.engine.ArchiveBinding(t.Context(), saved.ID, saved.Revision)
	if err != nil || !archived.Archived {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(moved, "file"))
	if err != nil || string(raw) != "keep" {
		t.Fatal("archive touched files", err)
	}
	root, err := syncfs.Open(moved)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := root.Probe()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	binding := saved.Binding
	binding.LocalID = caps.DirectoryID
	if _, err = f.engine.State.Register(binding, moved); err != nil {
		t.Fatal("reselection failed", err)
	}
}

func TestSyncLifecycleChangedRulesRequireNewPreview(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "keep", "bytes")
	saved := engineRound(t, f)
	for _, directory := range []string{f.local, f.remote} {
		writeEngineFile(t, directory, ".agentboxignore", "*.log\n")
	}
	if _, err := f.engine.Preview(t.Context(), saved.ID, syncclient.Automatic); !errors.Is(err, syncclient.ErrRulesChanged) {
		t.Fatal(err)
	}
	if _, err := f.engine.ArchiveBinding(t.Context(), saved.ID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.engine.State.Register(saved.Binding, saved.Directory)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := f.engine.Preview(t.Context(), fresh.ID, syncclient.Automatic)
	if err != nil || !preview.Plan.NeedsConfirmation {
		t.Fatal("new rules not confirmed", err)
	}
	if _, err = f.engine.Apply(t.Context(), fresh.ID, preview, preview.Plan.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestSyncLifecycleChangedServerPreservesLocalHistoryButRejectsRemoteRecovery(t *testing.T) {
	var changed atomic.Bool
	var requests atomic.Int32
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if changed.Load() && r.URL.Path == "/api/clients/capabilities" {
				rr := httptest.NewRecorder()
				next.ServeHTTP(rr, r)
				var identity syncproto.ServerIdentity
				if json.Unmarshal(rr.Body.Bytes(), &identity) != nil {
					t.Error("identity fixture")
					w.WriteHeader(500)
					return
				}
				identity.ServerID = syncproto.HashBytes([]byte("replacement"))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(identity)
				return
			}
			if changed.Load() {
				requests.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "remote-copy", "old remote")
	writeEngineFile(t, f.remote, "local-copy", "old local")
	engineRound(t, f)
	writeEngineFile(t, f.local, "remote-copy", "new remote")
	writeEngineFile(t, f.remote, "local-copy", "new local")
	saved := engineRound(t, f)
	changed.Store(true)
	history, err := f.engine.RecoveryHistory(t.Context(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range history[0].Files {
		destination, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		name, err := f.engine.ExportRecovery(t.Context(), saved.ID, history[0].ID, file.ID, destination)
		if file.Side == "remote" {
			if !errors.Is(err, syncclient.ErrBinding) {
				t.Fatal("old remote reference used on new server", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(destination, name))
			if err != nil || string(raw) != "old local" {
				t.Fatal("local history lost", err)
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("replacement server received old recovery references")
	}
	if _, err = f.engine.ArchiveBinding(t.Context(), saved.ID, saved.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestSyncLifecycleWrongUserCannotArchiveOrReadHistory(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	remote, err := syncclient.NewRemote(f.saved.Binding.Server, "client-fixture-bob", false)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	other := syncclient.Engine{State: f.engine.State, Remote: remote}
	if _, err = other.ArchiveBinding(t.Context(), f.saved.ID, f.saved.Revision); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal(err)
	}
	if _, err = other.RecoveryHistory(t.Context(), f.saved.ID); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal(err)
	}
}
