package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/collaboration"
	"agentbox/internal/safefs"
)

func TestCollaborationSnapshotRegistrationIsOwnedImmutableAndIdempotent(t *testing.T) {
	s, u, in, cfg := collaborationFixture(t)
	r, _, err := s.CreateCollaboration(u, strings.Repeat("a", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	project, output := t.TempDir(), t.TempDir()
	if err = os.WriteFile(filepath.Join(project, "file"), []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	source, err := safefs.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dest, err := safefs.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	snapshot, err := collaboration.PublishSnapshot(t.Context(), source, dest, strings.Repeat("b", 32), collaboration.SnapshotLimits{MaxBytes: 1024, MaxEntries: 10})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.StorageKey != "snapshots/"+snapshot.ID || artifact.State != "ready" || artifact.SHA256 != snapshot.ManifestSHA256 {
		t.Fatal(artifact)
	}
	replayed, err := s.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, snapshot)
	if err != nil || replayed != artifact {
		t.Fatal(replayed, err)
	}
	altered := snapshot
	altered.ManifestSHA256 = strings.Repeat("0", 64)
	if _, err = s.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, altered); !errors.Is(err, ErrChatConflict) {
		t.Fatal("artifact overwritten", err)
	}
	foreign := u
	foreign.Name = "bob"
	if _, err = s.CollaborationArtifact(foreign, r.ID, artifact.ID); err == nil {
		t.Fatal("foreign artifact read")
	}
	if err = s.DeleteUser(u.Name); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CollaborationArtifact(u, r.ID, artifact.ID); err == nil {
		t.Fatal("deleted owner artifact read")
	}
}
