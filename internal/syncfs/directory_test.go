package syncfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"agentbox/internal/syncproto"
)

func TestDirectoryDeletionKeepsProjectWideRecovery(t *testing.T) {
	root, dir := fixtureRoot(t)
	w := Writer{Root: root}
	for _, name := range []string{"parent", "parent/child"} {
		if err := w.Mkdir(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	old := fileEntry("old\r\n")
	if _, err := w.Replace(t.Context(), "parent/child/file", nil, old, bytes.NewBufferString("old\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Rmdir(t.Context(), "parent/child"); err == nil {
		t.Fatal("nonempty directory removed")
	}
	recovery, err := w.Delete(t.Context(), "parent/child/file", old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(recovery.Path, ".agentbox-sync/recovery/") {
		t.Fatal("recovery remained in child", recovery)
	}
	for _, name := range []string{"parent/child", "parent"} {
		if err = w.Rmdir(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(recovery.Path))); err != nil || string(data) != "old\r\n" {
		t.Fatal("removed ancestor lost recovery", err)
	}
	rules, _ := syncproto.ParseRules("")
	m, err := root.Scan(t.Context(), rules, Limits{})
	if err != nil || len(m.Entries) != 0 {
		t.Fatal("recovery leaked into manifest", err)
	}
}
func TestDirectoriesRefuseExistingIgnoredChildrenAndRacedFile(t *testing.T) {
	root, dir := fixtureRoot(t)
	w := Writer{Root: root}
	if err := w.Mkdir(t.Context(), "folder"); err != nil {
		t.Fatal(err)
	}
	if err := w.Mkdir(t.Context(), "folder"); err == nil {
		t.Fatal("existing directory treated as creation")
	}
	if err := os.WriteFile(filepath.Join(dir, "folder", ".git"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Rmdir(t.Context(), "folder"); err == nil {
		t.Fatal("ignored child recursively deleted")
	}
	if err := os.Remove(filepath.Join(dir, "folder", ".git")); err != nil {
		t.Fatal(err)
	}
	w.Guard = func(context.Context) error {
		if err := os.Remove(filepath.Join(dir, "folder")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "folder"), []byte("replacement file"), 0600)
	}
	if err := w.Rmdir(t.Context(), "folder"); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("replacement not refused", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "folder")); err != nil || string(data) != "replacement file" {
		t.Fatal("raced file deleted", err)
	}
}
func TestWriteRefusesMovedParentAndLeaseLostAfterTransfer(t *testing.T) {
	for _, kind := range []string{"parent", "lease"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := fixtureRoot(t)
			w := Writer{Root: root}
			if err := w.Mkdir(t.Context(), "sub"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sub", "file"), []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "lease" {
				w.Guard = func(context.Context) error { return syncproto.ErrLeaseExpired }
			}
			parentMoveBlocked := false
			source := &callbackReader{reader: bytes.NewBufferString("new"), fn: func() {
				if kind == "parent" {
					if err := os.Rename(filepath.Join(dir, "sub"), filepath.Join(dir, "moved")); err != nil {
						// Windows may fence the ancestor move while the staged file
						// is open. Exercise that kernel protection instead of assuming
						// Unix rename semantics or skipping the Windows case.
						if runtime.GOOS == "windows" && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(32))) { // ERROR_SHARING_VIOLATION
							parentMoveBlocked = true
							return
						}
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}}
			old := fileEntry("old")
			recovery, err := w.Replace(t.Context(), "sub/file", &old, fileEntry("new"), source)
			if parentMoveBlocked {
				if err != nil {
					t.Fatal("write with an unmoved, protected parent failed", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "moved")); !os.IsNotExist(err) {
					t.Fatal("denied ancestor move still created a destination", err)
				}
				if data, err := os.ReadFile(filepath.Join(dir, "sub", "file")); err != nil || string(data) != "new" {
					t.Fatal("protected parent did not receive the intended replacement", err)
				}
				if recovery.Path == "" {
					t.Fatal("protected-parent replacement lost before reference")
				}
				if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(recovery.Path))); err != nil || string(data) != "old" {
					t.Fatal("protected-parent replacement lost original bytes", err)
				}
				// Prove this was a live-handle fence, not a test directory that
				// cannot be renamed at all because of unrelated permissions.
				if err := os.Rename(filepath.Join(dir, "sub"), filepath.Join(dir, "moved")); err != nil {
					t.Fatal("parent remained locked after writer closed its handles", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
					t.Fatal("completed parent move retained the original directory", err)
				}
				if data, err := os.ReadFile(filepath.Join(dir, "moved", "file")); err != nil || string(data) != "new" {
					t.Fatal("post-close parent move lost the published file", err)
				}
				t.Log("Windows blocked the ancestor move during transfer; safe publication, recovery and post-close rename verified")
				return
			}
			if err == nil {
				t.Fatal("unsafe publication accepted")
			}
			parent := "sub"
			if kind == "parent" {
				parent = "moved"
			}
			if data, err := os.ReadFile(filepath.Join(dir, parent, "file")); err != nil || string(data) != "old" {
				t.Fatal("original changed", err)
			}
		})
	}
}
func TestRecoveryMustBeRecordedBeforePublication(t *testing.T) {
	root, dir := fixtureRoot(t)
	w := Writer{Root: root}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	var recorded Recovery
	w.RecoveryReady = func(recovery Recovery) error {
		recorded = recovery
		if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(recovery.Path))); err != nil || string(data) != "old" {
			t.Fatal("callback before durable copy", err)
		}
		return errors.New("state database unavailable")
	}
	old := fileEntry("old")
	if _, err := w.Replace(t.Context(), "file", &old, fileEntry("new"), bytes.NewBufferString("new")); err == nil {
		t.Fatal("publication ignored failed state journal")
	}
	if recorded.Path == "" {
		t.Fatal("missing recovery callback")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "file")); err != nil || string(data) != "old" {
		t.Fatal("original changed", err)
	}
}
func TestNativeRemovalCannotSwitchFileAndDirectoryTypes(t *testing.T) {
	root, dir := fixtureRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := root.root.Lstat("entry")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "entry")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "entry"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = removeTyped(root.root, "entry", info, false); err == nil {
		t.Fatal("file deletion removed directory")
	}
	directory, err := root.root.Lstat("entry")
	if err != nil || !directory.IsDir() {
		t.Fatal("directory lost", err)
	}
	if err = os.Remove(filepath.Join(dir, "entry")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "entry"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = removeTyped(root.root, "entry", directory, true); err == nil {
		t.Fatal("directory deletion removed file")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "entry")); err != nil || string(data) != "new" {
		t.Fatal("replacement file lost", err)
	}
}
