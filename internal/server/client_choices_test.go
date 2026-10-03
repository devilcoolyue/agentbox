package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncclient"
)

func TestSyncPerFileChoicesPreserveBothSourcesAndRecovery(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	for _, name := range []string{"a", "b", "deleted"} {
		writeEngineFile(t, f.local, name, "base")
	}
	engineRound(t, f)
	for _, name := range []string{"a", "b"} {
		writeEngineFile(t, f.local, name, "local-"+name)
		writeEngineFile(t, f.remote, name, "remote-"+name)
	}
	if err := os.Remove(filepath.Join(f.local, "deleted")); err != nil {
		t.Fatal(err)
	}
	writeEngineFile(t, f.remote, "deleted", "remote-edited")
	base, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil || len(base.Plan.Conflicts) != 3 {
		t.Fatal(base, err)
	}
	choices := map[string]syncclient.Direction{"a": syncclient.PreferLocal, "b": syncclient.PreferRemote}
	partial, err := f.engine.PreviewChoices(t.Context(), f.saved.ID, syncclient.Automatic, choices, base.Plan.Digest)
	if err != nil || len(partial.Plan.Conflicts) != 1 {
		t.Fatal(partial, err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, partial, partial.Plan.Digest); err == nil {
		t.Fatal("partial conflict resolution wrote files")
	}
	choices["deleted"] = syncclient.PreferLocal
	preview, err := f.engine.PreviewChoices(t.Context(), f.saved.ID, syncclient.Automatic, choices, base.Plan.Digest)
	if err != nil || len(preview.Plan.Conflicts) != 0 || len(preview.Plan.Operations) != 3 {
		t.Fatal(preview, err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, ""); err == nil {
		t.Fatal("choices bypassed explicit confirmation")
	}
	saved, err := f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest)
	if err != nil || saved.Pending != nil {
		t.Fatal(err)
	}
	for _, root := range []string{f.local, f.remote} {
		for name, want := range map[string]string{"a": "local-a", "b": "remote-b"} {
			raw, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(raw) != want {
				t.Fatalf("%s: %s %v", name, raw, err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "deleted")); !os.IsNotExist(err) {
			t.Fatal("selected deletion not applied", err)
		}
	}
	page, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || len(page.History[0].Files) != 3 || page.LocalRecovery == nil || page.LocalRecovery.Files != 1 || page.LocalRecovery.Bytes != int64(len("local-b")) {
		t.Fatal(page, err)
	}
	export, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "remote-a", "b": "local-b", "deleted": "remote-edited"}
	for _, file := range page.History[0].Files {
		name, err := f.engine.ExportRecovery(t.Context(), saved.ID, page.History[0].ID, file.ID, export)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(export, name))
		if err != nil || string(raw) != want[file.Path] {
			t.Fatal("lost overwritten version", file, err)
		}
	}
}

func TestSyncPerFileChoicesRejectStaleSelectionAndApplication(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "a", "local")
	writeEngineFile(t, f.remote, "a", "remote")
	base, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	choices := map[string]syncclient.Direction{"a": syncclient.PreferLocal}
	writeEngineFile(t, f.remote, "a", "remote-changed")
	if _, err = f.engine.PreviewChoices(t.Context(), f.saved.ID, syncclient.Automatic, choices, base.Plan.Digest); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("stale choice accepted", err)
	}
	base, err = f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := f.engine.PreviewChoices(t.Context(), f.saved.ID, syncclient.Automatic, choices, base.Plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	writeEngineFile(t, f.local, "a", "local-changed-again")
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("stale application accepted", err)
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending != nil || saved.Baseline != nil {
		t.Fatal("stale request changed state", err)
	}
}
