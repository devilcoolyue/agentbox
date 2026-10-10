package storagepolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheReclamationPreservesPrivateDataAndLinks(t *testing.T) {
	root := t.TempDir()
	owner := strings.Repeat("a", 64)
	cache := filepath.Join(root, "private", owner, "cache", "linux-amd64-debian-12")
	for _, kind := range []string{"go-build", "npm/_cacache", "go-mod", "playwright", "puppeteer"} {
		dir := filepath.Join(cache, kind)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte(strings.Repeat("x", 100)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cache, "go-build", "link")); err != nil {
		t.Fatal(err)
	}
	removed, err := CleanCaches(t.Context(), root, owner, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatal(removed)
	}
	for _, p := range []string{filepath.Join(outside, "keep"), filepath.Join(cache, "go-mod", "data"), filepath.Join(cache, "playwright", "data"), filepath.Join(cache, "puppeteer", "data")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("removed persistent data", p, err)
		}
	}
	if _, err := CleanCaches(t.Context(), root, "../escape", 0, time.Hour); err == nil {
		t.Fatal("invalid owner accepted")
	}
}
