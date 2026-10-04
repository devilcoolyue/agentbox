//go:build linux

package syncfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Run only in a dedicated small tmpfs. The explicit path is never inferred
// from a user's project or the machine's normal temporary filesystem.
func TestLinuxDiskFullPreservesOriginalAndRecovery(t *testing.T) {
	base := os.Getenv("AGENTBOX_SYNC_FULL_TEST_ROOT")
	if base == "" {
		t.Skip("dedicated small tmpfs required")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(base, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Type != 0x01021994 || stat.Blocks*uint64(stat.Bsize) > 8<<20 {
		t.Fatal("requires tmpfs of at most 8 MiB")
	}
	directory, err := os.MkdirTemp(base, "sync-full-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	root, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	writer := Writer{Root: root}
	old := fileEntry("original\r\n\x00")
	if _, err = writer.Replace(t.Context(), "file", nil, old, bytes.NewBufferString("original\r\n\x00")); err != nil {
		t.Fatal(err)
	}
	filler, err := os.Create(filepath.Join(directory, "fill"))
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 64<<10)
	for written := 0; written <= 8<<20; written += len(block) {
		if _, err = filler.Write(block); err != nil {
			break
		}
	}
	filler.Close()
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("failed to induce actual ENOSPC", err)
	}
	desired := fileEntry("new content")
	if _, err = writer.Replace(t.Context(), "file", &old, desired, bytes.NewBufferString("new content")); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("replace did not report full disk", err)
	}
	if raw, err := os.ReadFile(filepath.Join(directory, "file")); err != nil || string(raw) != "original\r\n\x00" {
		t.Fatal("full disk damaged original", err)
	}
	if _, err = writer.Delete(t.Context(), "file", old); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("delete bypassed full recovery storage", err)
	}
	if raw, err := os.ReadFile(filepath.Join(directory, "file")); err != nil || string(raw) != "original\r\n\x00" {
		t.Fatal("delete lost original", err)
	}
	if err = os.Remove(filepath.Join(directory, "fill")); err != nil {
		t.Fatal(err)
	}
	recovery, err := writer.Replace(t.Context(), "file", &old, desired, bytes.NewBufferString("new content"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := root.OpenFile(recovery.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	raw, err := io.ReadAll(before)
	if err != nil || string(raw) != "original\r\n\x00" {
		t.Fatal("successful retry lost recovery", err)
	}
}
