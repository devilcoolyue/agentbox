package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/collaboration"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

func TestCollaborationReviewEvidencePinsVerifiedArtifactsAndOwner(t *testing.T) {
	s, source, _ := clientTestServer(t)
	u, _ := s.store.GetUser(source.User)
	roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}, Executor: store.CollaborationBinding{AgentType: "codex", AccountID: "shared", Model: "fixture"}}
	in := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: source.ID, Text: "fixture", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: roles, MaxSteps: 20}
	run, _, err := s.store.CreateCollaboration(u, store.NewCollaborationID(), in, store.CollaborationConfig{Roles: roles, MaxSteps: 20})
	if err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(s.collaborationDir(u, run.ID), "snapshots")
	if err = os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(storage)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	db, err := sql.Open("sqlite", filepath.Join(s.cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Real, verified snapshot publications plus synthetic artifact registrations
	// exercise read authorization and pinning without invoking any model/runtime.
	for index, text := range []string{"old\n", "new\n", "latest-must-not-substitute\n"} {
		dir := t.TempDir()
		if err = os.WriteFile(filepath.Join(dir, "app.txt"), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		source, err := safefs.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat(string(rune('a'+index)), 32)
		snapshot, err := collaboration.PublishSnapshot(t.Context(), source, root, id, collaborationSnapshotLimits)
		source.Close()
		if err != nil {
			t.Fatal(err)
		}
		kind := "snapshot"
		if index == 0 {
			kind = "baseline"
		}
		if _, err = db.Exec(`INSERT INTO collaboration_artifacts(id,run_id,step_id,kind,storage_key,sha256,size_bytes,state,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, run.ID, "", kind, "snapshots/"+id, snapshot.ManifestSHA256, snapshot.Manifest.Bytes, "ready", "2026-10-12T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("UPDATE collaborations SET snapshot_id=?,plan_version=1 WHERE id=?", strings.Repeat("c", 32), run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = s.store.Collaboration(u, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := s.collaborationReviewEvidence(t.Context(), u, run, strings.Repeat("b", 32))
	if err != nil || !evidence.Complete || len(evidence.Changes) != 1 || !strings.Contains(evidence.Changes[0].Diff, "+new\n") || strings.Contains(evidence.Changes[0].Diff, "latest-must-not") {
		t.Fatal(evidence, err)
	}
	other := u
	other.Name = "other-owner"
	if _, err = s.collaborationReviewEvidence(t.Context(), other, run, strings.Repeat("b", 32)); err == nil {
		t.Fatal("foreign owner obtained code")
	}
	baseline := filepath.Join(storage, strings.Repeat("a", 32), "tree", "app.txt")
	if err = os.Chmod(baseline, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(baseline, []byte("bad\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(baseline, 0444); err != nil {
		t.Fatal(err)
	}
	if _, err = s.collaborationReviewEvidence(t.Context(), u, run, strings.Repeat("b", 32)); err == nil {
		t.Fatal("tampered baseline accepted")
	}
}
