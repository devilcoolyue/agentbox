package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

func TestBackupRestoreCollaborationIsolationAndScope(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, running := range []bool{false, true} {
			t.Run(fmt.Sprintf("full=%t/running=%t", full, running), func(t *testing.T) {
				configPath, data := fixture(t)
				st, err := store.Open(filepath.Join(data, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				u, _ := st.GetUser("alice")
				roles := store.CollaborationRoles{
					Coordinator: store.CollaborationBinding{AgentType: "codex", AccountID: "main", Model: "planner"},
					Executor:    store.CollaborationBinding{AgentType: "claude", AccountID: "worker", Model: "builder"},
				}
				cfg := store.CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2}
				in := store.CollaborationCreateInput{Scope: "original-scope", SourceSessionID: "s1", Text: "restore fixture", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: roles, MaxSteps: 20, MaxRepairs: 2}
				r, _, err := st.CreateCollaboration(u, store.NewCollaborationID(), in, cfg)
				if err != nil {
					t.Fatal(err)
				}
				r, err = st.BeginCollaborationPreparation(u, r.ID, r.Revision)
				if err != nil {
					t.Fatal(err)
				}
				taskDir := filepath.Join(data, "users", u.Name, "collaborations", r.ID)
				for _, dir := range []string{"work", "snapshots"} {
					if err = os.MkdirAll(filepath.Join(taskDir, dir), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err = os.WriteFile(filepath.Join(taskDir, "work/source"), []byte("preserved source\n"), 0600); err != nil {
					t.Fatal(err)
				}
				work, err := safefs.Open(filepath.Join(taskDir, "work"))
				if err != nil {
					t.Fatal(err)
				}
				defer work.Close()
				snapshots, err := safefs.Open(filepath.Join(taskDir, "snapshots"))
				if err != nil {
					t.Fatal(err)
				}
				defer snapshots.Close()
				limits := collaboration.SnapshotLimits{MaxBytes: 1 << 20, MaxEntries: 100}
				snapshot, err := collaboration.PublishSnapshot(t.Context(), work, snapshots, store.NewCollaborationID(), limits)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = st.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, snapshot); err != nil {
					t.Fatal(err)
				}
				planner := store.Session{ID: "planner", User: u.Name, Agent: "codex", AccountID: "main", DefaultModel: "planner", Status: store.StatusStopped, CreatedAt: time.Now()}
				worker := store.Session{ID: "worker", User: u.Name, Agent: "claude", AccountID: "worker", DefaultModel: "builder", Status: store.StatusStopped, CreatedAt: time.Now()}
				var step store.CollaborationStep
				r, step, err = st.PublishCollaborationParticipants(u, r.ID, r.Revision, planner, worker, store.CollaborationStepInput{Text: "Plan", SnapshotID: snapshot.ID})
				if err != nil {
					t.Fatal(err)
				}
				if running {
					step, err = st.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision)
					if err != nil {
						t.Fatal(err)
					}
					step, err = st.AdvanceCollaborationStep(u, r.ID, step.ID, step.Revision, "running")
					if err != nil {
						t.Fatal(err)
					}
				}
				planner.ContainerID = "original-instance-container"
				if err = st.Put(planner); err != nil {
					t.Fatal(err)
				}
				archive := filepath.Join(t.TempDir(), "backup.tar.gz")
				if _, err = Create(t.Context(), Options{Config: configPath, Output: archive, Full: full, CheckStopped: func(context.Context, []string) error { return nil }}); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "restored")
				if _, err = Restore(t.Context(), archive, target); err != nil {
					t.Fatal(err)
				}
				assertStandaloneRestoredDB(t, filepath.Join(target, "data/state.db"))
				restored, err := store.Open(filepath.Join(target, "data/state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer restored.Close()
				if err = restored.BindCollaborationScope(u, "restored-scope"); err != nil {
					t.Fatal(err)
				}
				got, err := restored.Collaboration(u, r.ID)
				if err != nil || got.Request.Scope != in.Scope || got.Scope != "restored-scope" || got.RequestID != r.RequestID {
					t.Fatal("frozen input/scope lost", got, err)
				}
				original, _ := st.Get(planner.ID)
				copy, _ := restored.Get(planner.ID)
				if original.ContainerID == "" || copy.ContainerID != "" {
					t.Fatal("restore did not detach source runtime", original, copy)
				}
				if _, fresh, err := restored.CreateCollaboration(u, r.RequestID, in, cfg); err != nil || fresh {
					t.Fatal("restored request was re-executed", fresh, err)
				}
				if _, _, err = restored.ApplyCollaborationAction(u, r.ID, store.NewCollaborationID(), store.CollaborationActionInput{Scope: in.Scope, ExpectedRevision: got.Revision, Action: "cancel"}); err == nil {
					t.Fatal("original instance scope accepted")
				}
				if !full {
					if got.State != store.CollabFailed || got.WaitReason != "artifacts_missing" {
						t.Fatal(got)
					}
					if _, err = restored.CollaborationArtifact(u, r.ID, snapshot.ID); err == nil {
						t.Fatal("missing project advertised ready")
					}
					if _, err = os.Stat(filepath.Join(target, "data/users", u.Name, "collaborations", r.ID)); !os.IsNotExist(err) {
						t.Fatal("system backup unexpectedly copied workspace", err)
					}
					return
				}
				root, err := safefs.Open(filepath.Join(target, "data/users", u.Name, "collaborations", r.ID, "snapshots"))
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if _, err = collaboration.VerifySnapshot(t.Context(), root, snapshot.ID, snapshot.ManifestSHA256, limits); err != nil {
					t.Fatal(err)
				}
				if running {
					if got.State != store.CollabUncertain {
						t.Fatal("unknown model outcome was guessed", got)
					}
					unknown, err := restored.CollaborationStep(step.ID)
					if err != nil {
						t.Fatal(err)
					}
					payload, _ := json.Marshal(map[string]any{"step_id": unknown.ID, "step_revision": unknown.Revision, "acknowledged": true})
					if _, _, err = restored.ApplyCollaborationAction(u, r.ID, store.NewCollaborationID(), store.CollaborationActionInput{Scope: got.Scope, ExpectedRevision: got.Revision, Action: "review", Payload: payload}); err != nil {
						t.Fatal(err)
					}
					got, err = restored.Collaboration(u, r.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				payload, _ := json.Marshal(cfg)
				if _, _, err = restored.ApplyCollaborationAction(u, r.ID, store.NewCollaborationID(), store.CollaborationActionInput{Scope: got.Scope, ExpectedRevision: got.Revision, Action: "configure", Payload: payload}); err != nil {
					t.Fatal("cannot reconfigure restored run", err)
				}
				got, err = restored.Collaboration(u, r.ID)
				if err != nil || got.ConfigVersion != r.ConfigVersion+1 || got.State != store.CollabAccepted {
					t.Fatal("restored participants reused", got, err)
				}
			})
		}
	}
}
