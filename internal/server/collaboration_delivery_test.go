package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

func TestCollaborationVerificationMissingArtifactFinalizesClaim(t *testing.T) {
	s, source, _ := clientTestServer(t)
	u, _ := s.store.GetUser(source.User)
	roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}, Executor: store.CollaborationBinding{AgentType: "codex", AccountID: "shared", Model: "fixture"}}
	in := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: source.ID, Text: "fixture", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: roles, MaxSteps: 20, VerificationCommand: "true"}
	cfg := store.CollaborationConfig{Roles: roles, MaxSteps: 20, VerificationCommand: "true"}
	r, _, err := s.store.CreateCollaboration(u, store.NewCollaborationID(), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Synthesize a reviewed DB snapshot whose filesystem publication was lost.
	db, err := sql.Open("sqlite", filepath.Join(s.cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE collaborations SET state='paused',wait_reason='verification_required',snapshot_id=? WHERE id=?", strings.Repeat("b", 32), r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = s.store.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.verifyCollaboration(t.Context(), u, r); err == nil {
		t.Fatal("missing artifact delivered")
	}
	var state string
	if err = db.QueryRow("SELECT state FROM collaboration_verifications WHERE run_id=?", r.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatal("claim orphaned before helper launch", state, err)
	}
	if _, fresh, err := s.store.BeginCollaborationVerification(u, r.ID, r.Revision); err != nil || fresh {
		t.Fatal("read retry grants execution", fresh, err)
	}
}

func TestCollaborationVerificationQueuesLegacyAndReviewedOrDeliversDirect(t *testing.T) {
	for _, mode := range []string{"legacy", "reviewed", "direct"} {
		t.Run(mode, func(t *testing.T) {
			s, source, _ := clientTestServer(t)
			u, _ := s.store.GetUser(source.User)
			roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}, Executor: store.CollaborationBinding{AgentType: "codex", AccountID: "shared", Model: "fixture"}}
			var workflow *collaboration.Workflow
			if mode != "legacy" {
				workflow = &collaboration.Workflow{Version: 1, Mode: mode}
			}
			in := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: source.ID, Text: "fixture", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: roles, MaxSteps: 20, Workflow: workflow}
			cfg := store.CollaborationConfig{Roles: roles, MaxSteps: 20, Workflow: workflow}
			r, _, err := s.store.CreateCollaboration(u, store.NewCollaborationID(), in, cfg)
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.store.BeginCollaborationPreparation(u, r.ID, r.Revision)
			if err != nil {
				t.Fatal(err)
			}
			storage := filepath.Join(s.collaborationDir(u, r.ID), "snapshots")
			if err = os.MkdirAll(storage, 0700); err != nil {
				t.Fatal(err)
			}
			root, err := safefs.Open(storage)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			project, err := safefs.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer project.Close()
			snapshot, err := collaboration.PublishSnapshot(t.Context(), project, root, store.NewCollaborationID(), collaborationSnapshotLimits)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.store.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, snapshot); err != nil {
				t.Fatal(err)
			}
			main := store.Session{ID: store.NewCollaborationID(), User: u.Name, Agent: "claude", AccountID: "shared", DefaultModel: "fixture", Status: store.StatusStopped, CreatedAt: time.Now()}
			worker := main
			worker.ID = store.NewCollaborationID()
			worker.Agent = "codex"
			var step store.CollaborationStep
			r, step, err = s.store.PublishCollaborationParticipants(u, r.ID, r.Revision, main, worker, store.CollaborationStepInput{Text: "fixture plan", SnapshotID: snapshot.ID})
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(s.cfg.DataDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			output, err := collaboration.PublishSnapshot(t.Context(), project, root, store.NewCollaborationID(), collaborationSnapshotLimits)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO collaboration_artifacts(id,run_id,step_id,kind,storage_key,sha256,size_bytes,state,created_at) VALUES(?,?,?,'snapshot',?,?,?,'ready','fixture')`, output.ID, r.ID, step.ID, "snapshots/"+output.ID, output.ManifestSHA256, output.Manifest.Bytes); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO collaboration_messages(run_id,seq,id,step_id,sender_role,type,config_version,plan_version,body_json,created_at) SELECT ?,COALESCE(MAX(seq),0)+1,?,?,'executor','result',1,1,?,'fixture' FROM collaboration_messages WHERE run_id=?`, r.ID, store.NewCollaborationID(), step.ID, `{"summary":"complete","completed":["task"],"remaining":[],"tests":[]}`, r.ID); err != nil {
				t.Fatal(err)
			}
			// Synthesize completed execution, keeping actual immutable snapshot
			// and participant registrations to exercise the verification dispatch.
			if _, err = db.Exec("UPDATE collaboration_steps SET state='completed' WHERE run_id=?", r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("UPDATE collaborations SET state='paused',wait_reason='verification_required',snapshot_id=?,plan_version=1 WHERE id=?", output.ID, r.ID); err != nil {
				t.Fatal(err)
			}
			r, err = s.store.Collaboration(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.verifyCollaboration(t.Context(), u, r); err != nil {
				t.Fatal("verification did not advance", err)
			}
			r, err = s.store.Collaboration(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "direct" {
				if r.State != store.CollabPaused || r.WaitReason != "delivery_required" {
					t.Fatal("direct did not reach delivery", r.State, r.WaitReason)
				}
				return
			}
			work, err := s.store.CollaborationWork(u, r.ID)
			if err != nil || r.State != store.CollabReviewing || work.Step.Kind != "review" || !strings.Contains(work.Input.Text, `"state":"not_requested"`) {
				t.Fatal("review not queued", work, err)
			}
			if got := strings.Contains(work.Input.Text, `"review_evidence"`); got != (mode == "reviewed") {
				t.Fatal("incorrect evidence compatibility", mode, got)
			}
		})
	}
}
