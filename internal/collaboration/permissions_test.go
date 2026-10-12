package collaboration

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"agentbox/internal/safefs"
)

func TestCollaborationUmask0077(t *testing.T) {
	if os.Getenv("AGENTBOX_COLLAB_UMASK_HELPER") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCollaborationUmask0077$", "-test.v")
		cmd.Env = append(os.Environ(), "AGENTBOX_COLLAB_UMASK_HELPER=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated umask test: %v\n%s", err, out)
		}
		return
	}
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	project, storage, materialized := t.TempDir(), t.TempDir(), t.TempDir()
	source, err := safefs.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshots, err := safefs.Open(storage)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshots.Close()
	copy, err := safefs.Open(materialized)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	content := "fixture"
	limits := SnapshotLimits{MaxBytes: 1024, MaxEntries: 20}
	entries, err := ParseGitTree([]byte(fmt.Sprintf("100644 blob %s %d\tnested/deep/file\x00", fixtureBlob(content), len(content))), limits)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err = writer.WriteHeader(&tar.Header{Name: "nested/deep/file", Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = ImportGitArchive(t.Context(), &archive, source, entries, limits); err != nil {
		t.Fatal(err)
	}
	assertMode := func(path string, mode os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: metadata=%v err=%v; want %o", path, info, err, mode)
		}
	}
	for _, dir := range []string{"nested", "nested/deep"} {
		assertMode(filepath.Join(project, dir), 0755)
	}
	id := strings.Repeat("a", 32)
	snapshot, err := PublishSnapshot(t.Context(), source, snapshots, id, limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"tree", "tree/nested", "tree/nested/deep"} {
		assertMode(filepath.Join(storage, id, dir), 0755)
	}
	assertMode(storage, 0700)
	if _, err = VerifySnapshot(t.Context(), snapshots, id, snapshot.ManifestSHA256, limits); err != nil {
		t.Fatal(err)
	}
	tree, err := snapshots.Sub(id + "/tree")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err = MaterializeSnapshot(t.Context(), tree, copy, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"nested", "nested/deep"} {
		assertMode(filepath.Join(materialized, dir), 0755)
	}
	// No process-wide relaxation; unrelated private paths must remain private.
	private := filepath.Join(project, "private")
	if err = os.Mkdir(private, 0755); err != nil {
		t.Fatal(err)
	}
	assertMode(private, 0700)
	if err = os.Chmod(filepath.Join(storage, id, "tree"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifySnapshot(t.Context(), snapshots, id, snapshot.ManifestSHA256, limits); err == nil {
		t.Fatal("inaccessible snapshot root accepted")
	}
}
