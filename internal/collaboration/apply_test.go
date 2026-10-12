package collaboration

import (
	"agentbox/internal/safefs"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fileEntry(name, text string) SnapshotEntry {
	sum := sha256.Sum256([]byte(text))
	return SnapshotEntry{Path: name, Kind: "file", Mode: 0644, Size: int64(len(text)), SHA256: hex.EncodeToString(sum[:])}
}
func TestApplySnapshotStagesWithoutTouchingOriginal(t *testing.T) {
	if runtime.GOOS == "darwin" && os.Geteuid() != 0 {
		t.Skip("strict chown to container UID requires Linux root fixture")
	}
	source, stage, result := t.TempDir(), t.TempDir(), t.TempDir()
	for name, text := range map[string]string{"main.txt": "before", ".git/config": "git metadata", "ignored/cache": "preserve"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(result, "main.txt"), []byte("after"), 0644); err != nil {
		t.Fatal(err)
	}
	src, _ := safefs.Open(source)
	defer src.Close()
	dst, _ := safefs.Open(stage)
	defer dst.Close()
	tree, _ := safefs.Open(result)
	defer tree.Close()
	if err := CloneWorkspace(context.Background(), src, dst, 1<<20, 100); err != nil {
		t.Fatal(err)
	}
	before := Snapshot{Manifest: SnapshotManifest{Entries: []SnapshotEntry{fileEntry("main.txt", "before")}}}
	after := Snapshot{Manifest: SnapshotManifest{Entries: []SnapshotEntry{fileEntry("main.txt", "after")}}}
	if err := ApplySnapshot(context.Background(), dst, tree, before, after); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		root       *safefs.Root
		path, text string
	}{{src, "main.txt", "before"}, {dst, "main.txt", "after"}, {dst, ".git/config", "git metadata"}, {dst, "ignored/cache", "preserve"}} {
		b, err := check.root.ReadFile(check.path, 100)
		if err != nil || string(b) != check.text {
			t.Fatal(check.path, string(b), err)
		}
	}
	if err := ApplySnapshot(context.Background(), dst, tree, before, after); err == nil {
		t.Fatal("accepted changed source")
	}
}
func TestApplySnapshotRefusesIgnoredCollisionAndUnsafeLinks(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "conflict"), []byte("keep"), 0644)
	os.Symlink("/etc", filepath.Join(root, "external"))
	dir, _ := safefs.Open(root)
	defer dir.Close()
	before := Snapshot{}
	after := Snapshot{Manifest: SnapshotManifest{Entries: []SnapshotEntry{fileEntry("conflict", "result")}}}
	if err := ApplySnapshot(context.Background(), dir, dir, before, after); err == nil {
		t.Fatal("overwrote ignored file")
	}
	after.Manifest.Entries = []SnapshotEntry{fileEntry("external/passwd", "result")}
	if err := ApplySnapshot(context.Background(), dir, dir, before, after); err == nil {
		t.Fatal("followed external link")
	}
	b, _ := os.ReadFile(filepath.Join(root, "conflict"))
	if string(b) != "keep" {
		t.Fatal(string(b))
	}
}
