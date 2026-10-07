package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"agentbox/internal/store"
)

func TestBackupRestorePreservesCreationRecovery(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			cfg, data := fixture(t)
			st, err := store.Open(filepath.Join(data, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			u, _ := st.GetUser("alice")
			creations := make(map[string]store.WorkspaceCreation)
			for _, state := range []string{"reserved", "succeeded", "running"} {
				c, err := st.ReserveWorkspaceCreation(u, store.WorkspaceCreation{
					RequestID: state, Fingerprint: "original-request", Request: json.RawMessage(`{"source":"upload"}`),
					Session: store.Session{ID: "creation-" + state, User: u.Name, Agent: "codex", DefaultModel: "saved-model"},
				})
				if err != nil {
					t.Fatal(err)
				}
				creations[state] = c
				if state == "reserved" {
					continue
				}
				if _, err = st.CompleteWorkspaceCreation(u, c.RequestID); err != nil {
					t.Fatal(err)
				}
				if _, fresh, err := st.BeginWorkspaceImport(u, c.RequestID, store.WorkspaceImport{AttemptID: "attempt", Fingerprint: "original-files", Kind: "upload", Directory: "project"}); err != nil || !fresh {
					t.Fatal(fresh, err)
				}
				if state == "succeeded" {
					if err = st.FinishWorkspaceImport(u, c.RequestID, "attempt", state, json.RawMessage(`{"directory":"project","files":2}`)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err = st.AbandonUnknownWorkspaceCreation(u, "abandoned", "fenced-session"); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "backup.tar.gz")
			if _, err = Create(t.Context(), Options{Config: cfg, Output: archive, Full: full, CheckStopped: func(context.Context, []string) error { return nil }}); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "restored")
			if _, err = Restore(t.Context(), archive, target); err != nil {
				t.Fatal(err)
			}
			restored, err := store.Open(filepath.Join(target, "data/state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if err = restored.RecoverWorkspaceImports(); err != nil {
				t.Fatal(err)
			}
			for state, original := range creations {
				c, err := restored.ReserveWorkspaceCreation(u, original)
				if err != nil || c.Session.ID != original.Session.ID || c.Session.DefaultModel != "saved-model" {
					t.Fatalf("creation changed after restore: %+v %v", c, err)
				}
				if state == "reserved" {
					if c.State != "reserved" {
						t.Fatal("unfinished creation lost", c.State)
					}
					continue
				}
				i, fresh, err := restored.BeginWorkspaceImport(u, c.RequestID, store.WorkspaceImport{AttemptID: "attempt", Fingerprint: "original-files"})
				want := state
				if state == "running" {
					want = "uncertain"
				}
				if err != nil || fresh || i.State != want {
					t.Fatalf("import replay after restore: %+v fresh=%t err=%v", i, fresh, err)
				}
				if state == "running" {
					_, _, err = restored.BeginWorkspaceImport(u, c.RequestID, store.WorkspaceImport{AttemptID: "retry", Fingerprint: "retry"})
					if !errors.Is(err, store.ErrImportPending) {
						t.Fatal("restored uncertain result allowed retry", err)
					}
				} else if string(i.Result) != `{"directory":"project","files":2}` {
					t.Fatal("successful import result lost", string(i.Result))
				}
			}
			c, err := restored.WorkspaceCreation(u, "abandoned")
			if err != nil || c.State != "abandoned" {
				t.Fatal("abandon fence lost", c, err)
			}
		})
	}
}
