package collaboration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/safefs"
)

func TestSnapshotPublicationCapturesFilesModesAndRelativeLinks(t *testing.T) {
	project, storage := t.TempDir(), t.TempDir()
	for _, dir := range []string{"src", "empty", ".git"} {
		if err := os.Mkdir(filepath.Join(project, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "src", "run.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".git", "config"), []byte("must not export"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/run.sh", filepath.Join(project, "run")); err != nil {
		t.Fatal(err)
	}
	source, err := safefs.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := safefs.Open(storage)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	id := strings.Repeat("a", 32)
	limits := SnapshotLimits{MaxBytes: 1024, MaxEntries: 10}
	snapshot, err := PublishSnapshot(t.Context(), source, destination, id, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Manifest.Entries) != 4 || snapshot.Manifest.Bytes != 10 {
		t.Fatal(snapshot)
	}
	if _, err = VerifySnapshot(t.Context(), destination, id, snapshot.ManifestSHA256, limits); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(storage, id, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != snapshot.ManifestSHA256 {
		t.Fatal("manifest hash mismatch")
	}
	if _, err = os.Stat(filepath.Join(storage, id, "tree", ".git")); !os.IsNotExist(err) {
		t.Fatal("Git administration exported")
	}
	if target, err := os.Readlink(filepath.Join(storage, id, "tree", "run")); err != nil || target != "src/run.sh" {
		t.Fatal(target, err)
	}
	if info, err := os.Stat(filepath.Join(storage, id, "tree", "src", "run.sh")); err != nil || info.Mode().Perm() != 0555 {
		t.Fatal(info, err)
	}
	if _, err = PublishSnapshot(t.Context(), source, destination, id, limits); err == nil {
		t.Fatal("existing immutable snapshot replaced")
	}
	if err = os.WriteFile(filepath.Join(project, "src", "run.sh"), []byte("changed"), 0755); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(storage, id, "tree", "src", "run.sh")); err != nil || string(data) != "#!/bin/sh\n" {
		t.Fatal("source mutation changed published snapshot", err)
	}
}

func TestSnapshotVerificationRejectsTamperedRestoredFiles(t *testing.T) {
	for _, mutation := range []string{"digest", "content", "extra", "missing", "mode", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			project, storage := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(project, "file"), []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			source, err := safefs.Open(project)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			root, err := safefs.Open(storage)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			id := strings.Repeat("d", 32)
			limits := SnapshotLimits{MaxBytes: 1024, MaxEntries: 10}
			snapshot, err := PublishSnapshot(t.Context(), source, root, id, limits)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(storage, id, "tree", "file")
			switch mutation {
			case "digest":
				snapshot.ManifestSHA256 = strings.Repeat("0", 64)
			case "content":
				if err = os.Chmod(file, 0644); err == nil {
					err = os.WriteFile(file, []byte("tampered"), 0644)
				}
				if err == nil {
					err = os.Chmod(file, 0444)
				}
			case "extra":
				err = os.WriteFile(filepath.Join(storage, id, "tree", "extra"), []byte("x"), 0444)
			case "missing":
				err = os.Remove(file)
			case "mode":
				err = os.Chmod(file, 0555)
			case "symlink":
				if err = os.Remove(file); err == nil {
					err = os.Symlink(filepath.Join(project, "file"), file)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = VerifySnapshot(t.Context(), root, id, snapshot.ManifestSHA256, limits); err == nil {
				t.Fatal("tampered snapshot accepted")
			}
		})
	}
}

func TestSnapshotFailureNeverPublishesPartialTree(t *testing.T) {
	for _, kind := range []string{"bytes", "entries", "escape", "absolute", "hardlink", "cancel", "nested-repo", "link-expansion"} {
		t.Run(kind, func(t *testing.T) {
			project, storage := t.TempDir(), t.TempDir()
			limits := SnapshotLimits{MaxBytes: 1024, MaxEntries: 10}
			ctx := t.Context()
			if err := os.WriteFile(filepath.Join(project, "file"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "bytes":
				limits.MaxBytes = 1
			case "entries":
				limits.MaxEntries = 1
				if err := os.WriteFile(filepath.Join(project, "extra"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "escape":
				if err := os.Symlink("../secret", filepath.Join(project, "link")); err != nil {
					t.Fatal(err)
				}
			case "absolute":
				if err := os.Symlink("/etc/passwd", filepath.Join(project, "link")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(project, "file"), filepath.Join(project, "other")); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nested-repo":
				if err := os.MkdirAll(filepath.Join(project, "nested", ".git"), 0755); err != nil {
					t.Fatal(err)
				}
			case "link-expansion":
				if err := os.Mkdir(filepath.Join(project, "deep"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("..", filepath.Join(project, "deep", "back")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("deep/back/../secret", filepath.Join(project, "escape")); err != nil {
					t.Fatal(err)
				}
			}
			source, err := safefs.Open(project)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			dest, err := safefs.Open(storage)
			if err != nil {
				t.Fatal(err)
			}
			defer dest.Close()
			if _, err = PublishSnapshot(ctx, source, dest, strings.Repeat("b", 32), limits); err == nil {
				t.Fatal("unsafe or incomplete snapshot published")
			}
			entries, err := os.ReadDir(storage)
			if err != nil || len(entries) != 0 {
				t.Fatal("partial output survived", entries, err)
			}
		})
	}
}
