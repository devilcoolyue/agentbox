package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncclient"
)

func localRecoveryFixture(t *testing.T) (*engineFixture, syncclient.SavedBinding, syncclient.RecoveryBatch) {
	t.Helper()
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.remote, "file", "before")
	engineRound(t, f)
	writeEngineFile(t, f.remote, "file", "after")
	saved := engineRound(t, f)
	page, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || len(page.History[0].Files) != 1 || !page.History[0].Files[0].Disposable {
		t.Fatal(page, err)
	}
	return f, saved, page.History[0]
}

func TestSyncDiscardLocalRecoveryPreservesBaselineAndAudit(t *testing.T) {
	f, saved, batch := localRecoveryFixture(t)
	file := batch.Files[0]
	before, _ := json.Marshal(saved.Baseline)
	if _, err := f.engine.DiscardLocalRecovery(t.Context(), saved.ID, batch.ID, file.ID, saved.Revision-1); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("stale cleanup", err)
	}
	cleaned, err := f.engine.DiscardLocalRecovery(t.Context(), saved.ID, batch.ID, file.ID, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(cleaned.Baseline)
	if string(before) != string(after) {
		t.Fatal("baseline changed")
	}
	page, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || page.LocalRecovery.Files != 0 || page.LocalRecovery.Bytes != 0 || page.History[0].Files[0].State != "discarded" || page.History[0].Files[0].Disposable || page.History[0].Cleanable {
		t.Fatal(page, err)
	}
	if _, err = f.engine.ExportRecovery(t.Context(), saved.ID, batch.ID, file.ID, t.TempDir()); err == nil {
		t.Fatal("discarded copy advertised")
	}
	if data, err := os.ReadFile(filepath.Join(f.local, "file")); err != nil || string(data) != "after" {
		t.Fatal("project changed", err)
	}
	preview, err := f.engine.Preview(t.Context(), saved.ID, syncclient.Automatic)
	if err != nil || len(preview.Plan.Operations) != 0 {
		t.Fatal("cleanup changed sync plan", err)
	}
}

func TestSyncDiscardRecoveryResumesPersistedIntentAfterRestart(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry-original", true: "crash-after-unlink"}[missing], func(t *testing.T) {
			f, saved, batch := localRecoveryFixture(t)
			stored, err := f.engine.State.History(saved.ID, batch.ID)
			if err != nil {
				t.Fatal(err)
			}
			copyPath := filepath.Join(f.local, filepath.FromSlash(stored.Operations[0].Recovery.Path))
			if err = os.WriteFile(copyPath, []byte("external edit"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = f.engine.DiscardLocalRecovery(t.Context(), saved.ID, batch.ID, batch.Files[0].ID, saved.Revision); err == nil {
				t.Fatal("changed copy removed")
			}
			if missing {
				err = os.Remove(copyPath)
			} else {
				err = os.WriteFile(copyPath, []byte("before"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.engine.State.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := syncclient.OpenState(f.stateDir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			f.engine.State = reopened
			page, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
			if err != nil || page.History[0].Files[0].State != "discarding" {
				t.Fatal("lost durable intent", page, err)
			}
			if _, err = f.engine.DiscardLocalRecovery(t.Context(), saved.ID, batch.ID, batch.Files[0].ID, page.Revision); err != nil {
				t.Fatal(err)
			}
			page, err = f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
			if err != nil || page.History[0].Files[0].State != "discarded" {
				t.Fatal(page, err)
			}
		})
	}
}

func TestSyncDiscardRecoveryRefusesPendingAndForeignUser(t *testing.T) {
	f, saved, batch := localRecoveryFixture(t)
	remote, err := syncclient.NewRemote(saved.Binding.Server, "client-fixture-bob", false)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	other := syncclient.Engine{State: f.engine.State, Remote: remote}
	if _, err = other.DiscardLocalRecovery(t.Context(), saved.ID, batch.ID, batch.Files[0].ID, saved.Revision); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("foreign cleanup", err)
	}
	preview, err := f.engine.Preview(t.Context(), saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	// Persist an unstarted batch using the same state boundary as Apply.
	saved, err = f.engine.State.Begin(saved.ID, saved.Revision, preview.ProjectRevision, preview.Plan, preview.Plan.Digest, saved.Baseline.Local, saved.Baseline.Remote, preview.Options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.DiscardLocalRecovery(t.Context(), saved.ID, batch.ID, batch.Files[0].ID, saved.Revision); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("pending cleanup", err)
	}
}

func TestLocalRecoveryCleanupNeverDeletesRemoteCopy(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "file", "remote before")
	engineRound(t, f)
	writeEngineFile(t, f.local, "file", "remote after")
	saved := engineRound(t, f)
	page, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	batch := page.History[0]
	file := batch.Files[0]
	if file.Side != "remote" || file.Disposable {
		t.Fatal(file)
	}
	if _, err = f.engine.DiscardLocalRecovery(t.Context(), saved.ID, batch.ID, file.ID, saved.Revision); err == nil {
		t.Fatal("remote cleanup accepted")
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name, err := f.engine.ExportRecovery(t.Context(), saved.ID, batch.ID, file.ID, directory)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(directory, name)); err != nil || string(data) != "remote before" {
		t.Fatal("remote recovery lost", err)
	}
}
