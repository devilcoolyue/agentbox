package clientidentity

import (
	"agentbox/internal/syncproto"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestIdentityConcurrentCreationAndRestart(t *testing.T) {
	directory := t.TempDir()
	var workers sync.WaitGroup
	results := make(chan string, 12)
	for range 12 {
		workers.Go(func() {
			id, err := LoadOrCreate(directory)
			if err != nil {
				t.Error(err)
			}
			results <- id
		})
	}
	workers.Wait()
	close(results)
	id, err := LoadOrCreate(directory)
	if err != nil || !syncproto.ValidHash(id) {
		t.Fatal(id, err)
	}
	for result := range results {
		if result != id {
			t.Fatal("identity changed during concurrent creation")
		}
	}
	info, err := os.Stat(filepath.Join(directory, File))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("identity permissions", info, err)
	}
	other, err := LoadOrCreate(t.TempDir())
	if err != nil || other == id {
		t.Fatal("different installation reused identity", err)
	}
}
func TestIdentityCorruptionAndLinksAreNotRegenerated(t *testing.T) {
	for _, kind := range []string{"empty", "invalid", "oversized", "link", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, File)
			valid := syncproto.HashBytes([]byte("fixture")) + "\n"
			var err error
			switch kind {
			case "empty":
				err = os.WriteFile(target, nil, 0600)
			case "invalid":
				err = os.WriteFile(target, []byte("not-an-id"), 0600)
			case "oversized":
				err = os.WriteFile(target, []byte(valid+"x"), 0600)
			case "directory":
				err = os.Mkdir(target, 0700)
			default:
				other := filepath.Join(t.TempDir(), "identity")
				if err = os.WriteFile(other, []byte(valid), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "link" {
					err = os.Symlink(other, target)
				} else {
					err = os.Link(other, target)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.Lstat(target)
			if _, err = LoadOrCreate(dir); err == nil {
				t.Fatal("unsafe identity accepted")
			}
			after, _ := os.Lstat(target)
			if !os.SameFile(before, after) {
				t.Fatal("unsafe identity replaced")
			}
		})
	}
}
