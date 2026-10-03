package syncfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncproto"
)

func TestDiscardRecoveryOnlyDeletesVerifiedCopy(t *testing.T) {
	r, dir := fixtureRoot(t)
	w := Writer{Root: r}
	old, next := fileEntry("old"), fileEntry("next")
	if _, err := w.Replace(t.Context(), "file", nil, old, bytes.NewBufferString("old")); err != nil {
		t.Fatal(err)
	}
	copy, err := w.Replace(t.Context(), "file", &old, next, bytes.NewBufferString("next"))
	if err != nil {
		t.Fatal(err)
	}
	unsafe := copy
	unsafe.Path = "file"
	if err = r.DiscardRecovery(t.Context(), unsafe); !errors.Is(err, syncproto.ErrInvalid) {
		t.Fatal("arbitrary path", err)
	}
	wrong := copy
	wrong.Hash = next.Hash
	if err = r.DiscardRecovery(t.Context(), wrong); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("hash mismatch", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = r.DiscardRecovery(canceled, copy); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled deletion", err)
	}
	if err = r.DiscardRecovery(t.Context(), copy); err != nil {
		t.Fatal(err)
	}
	if err = r.DiscardRecovery(t.Context(), copy); err != nil {
		t.Fatal("retry", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "file")); err != nil || string(data) != "next" {
		t.Fatal("project modified", err)
	}
	usage, err := r.RecoveryUsage(t.Context())
	if err != nil || usage.Files != 0 || usage.Bytes != 0 {
		t.Fatal(usage, err)
	}
}

func TestDiscardRecoveryRejectsLinksAndReplacedRoot(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "root"} {
		t.Run(mode, func(t *testing.T) {
			r, dir := fixtureRoot(t)
			copy := Recovery{Path: ".agentbox-sync/recovery/test.bak", Hash: syncproto.HashBytes([]byte("old")), Size: 3}
			if err := os.MkdirAll(filepath.Join(dir, ".agentbox-sync", "recovery"), 0700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, filepath.FromSlash(copy.Path))
			var err error
			switch mode {
			case "symlink":
				err = os.Symlink(outside, name)
			case "hardlink":
				err = os.Link(outside, name)
			case "root":
				if err = os.WriteFile(name, []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Rename(dir, dir+"-moved")
				if err == nil {
					t.Cleanup(func() { _ = os.Rename(dir+"-moved", dir) })
				}
			}
			if err != nil {
				t.Skipf("platform cannot create %s fixture: %v", mode, err)
			}
			if err = r.DiscardRecovery(t.Context(), copy); err == nil {
				t.Fatal("unsafe copy discarded")
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "old" {
				t.Fatal("outside bytes changed", err)
			}
		})
	}
}
