package safefs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func testRoot(t *testing.T) (*Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func TestPinnedDirectorySurvivesReplacement(t *testing.T) {
	r, dir := testRoot(t)
	if err := r.Mkdir("project", 0o755); err != nil {
		t.Fatal(err)
	}
	sub, err := r.Sub("project")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "project"), filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "project")); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.WriteFile("secret", []byte("inside"), WriteOptions{Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || string(got) != "outside" {
		t.Fatalf("outside changed: %q %v", got, err)
	}
	if got, err := sub.ReadFile("secret", 100); err != nil || string(got) != "inside" {
		t.Fatalf("pinned root changed: %q %v", got, err)
	}
	if _, err := r.Sub("project"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reopened link: %v", err)
	}
}

func TestAtomicWriteDoesNotTruncateHardLinkOrPublishFailure(t *testing.T) {
	r, dir := testRoot(t)
	outside := filepath.Join(t.TempDir(), "original")
	if err := os.WriteFile(outside, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "file")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteFile("file", []byte("new"), WriteOptions{Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "old" {
		t.Fatalf("hard-linked inode overwritten: %q", got)
	}
	if _, err := r.WriteAtomic("file", strings.NewReader("too large"), WriteOptions{Mode: 0o644, MaxBytes: 1, Limit: true}); err == nil {
		t.Fatal("accepted excess bytes")
	}
	if got, _ := r.ReadFile("file", 100); string(got) != "new" {
		t.Fatalf("failed save replaced original: %q", got)
	}
	entries, err := r.ReadDir(".")
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v %v", entries, err)
	}
}

func TestRejectLinksAndSpecialFiles(t *testing.T) {
	r, dir := testRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "pipe", "../real", "/etc/passwd", "."} {
		if f, err := r.OpenFile(name); err == nil {
			f.Close()
			t.Fatalf("opened %q", name)
		}
		if _, err := r.WriteFile(name, []byte("replace"), WriteOptions{Mode: 0o644}); err == nil {
			t.Fatalf("overwrote %q", name)
		}
	}
}

// The goroutine swaps a legitimate file/directory for an outside symlink while
// real reads, writes, listings and deletes run. No test-only hooks bypass the
// filesystem: this exercises the TOCTOU window under actual concurrent rename.
func TestConcurrentSymlinkSwapsStayConfined(t *testing.T) {
	r, dir := testRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "value"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Mkdir("slot", 0o755); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(filepath.Join(dir, "slot"), filepath.Join(dir, "parked")) != nil {
				continue
			}
			_ = os.Symlink(outside, filepath.Join(dir, "slot"))
			_ = os.Remove(filepath.Join(dir, "slot"))
			_ = os.Rename(filepath.Join(dir, "parked"), filepath.Join(dir, "slot"))
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 300; i++ {
		_, _ = r.WriteFile("slot/value", []byte("inside"), WriteOptions{Mode: 0o644})
		if data, err := r.ReadFile("slot/value", 100); err == nil && strings.Contains(string(data), "outside-secret") {
			t.Fatal("read escaped")
		}
		_, _ = r.ReadDir("slot")
		_ = r.RemoveAll("slot/value")
	}
	if data, err := os.ReadFile(filepath.Join(outside, "value")); err != nil || string(data) != "outside-secret" {
		t.Fatalf("outside modified/deleted: %q %v", data, err)
	}
}

func TestRenameAcrossPinnedRootsPreservesConflicts(t *testing.T) {
	a, _ := testRoot(t)
	b, dir := testRoot(t)
	_, _ = a.WriteFile("file", []byte("a"), WriteOptions{Mode: 0o644})
	_, _ = b.WriteFile("file", []byte("b"), WriteOptions{Mode: 0o644})
	if err := a.RenameTo("file", b, "file", false); !os.IsExist(err) {
		t.Fatalf("conflict not preserved: %v", err)
	}
	if err := a.RenameTo("file", b, "moved", false); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "moved"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "a" {
		t.Fatalf("moved=%q", got)
	}
}
