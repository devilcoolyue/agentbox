package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageReportDoesNotFollowLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), make([]byte, 1000), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "users/alice"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "users/alice/file"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "users/alice/link")); err != nil {
		t.Fatal(err)
	}
	got := measureStorage(t.Context(), root)
	if got.Bytes != 5 || got.Users["alice"] != 5 || got.Partial {
		t.Fatalf("%+v", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !measureStorage(ctx, root).Partial {
		t.Fatal("cancelled report looks complete")
	}
}
