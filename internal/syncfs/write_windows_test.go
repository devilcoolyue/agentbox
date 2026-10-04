package syncfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agentbox/internal/syncproto"
	"golang.org/x/sys/windows"
)

func lockWindowsDestination(t *testing.T, file string) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

func lockWindowsStagedSource(t *testing.T, directory string) func() {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, ".agentbox-sync-tmp-*"))
	if err != nil || len(matches) != 1 {
		t.Fatal("missing staged replacement source", matches, err)
	}
	// Renameat first opens its source with DELETE access. Denying that access
	// reliably exercises sharing violation before either target-rename API;
	// an open destination may instead fail with permanent ACCESS_DENIED.
	return lockWindowsDestination(t, matches[0])
}

func TestWindowsReplaceRetriesOnlyExplicitSharingOrLockViolation(t *testing.T) {
	for _, failure := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION} {
		if !retryableReplaceError(&os.LinkError{Op: "rename", Old: "stage", New: "target", Err: failure}) {
			t.Fatal("wrapped Windows sharing error not recognized", failure)
		}
	}
	for _, failure := range []error{nil, windows.ERROR_ACCESS_DENIED, windows.ERROR_FILE_NOT_FOUND, windows.ERROR_ALREADY_EXISTS, windows.ERROR_DISK_FULL, context.Canceled, errors.New("sharing violation")} {
		if retryableReplaceError(failure) {
			t.Fatal("permanent or unknown error authorized a retry", failure)
		}
	}
}

func TestWindowsTransientSharingRetriesAfterReleaseAndKeepsOneRecovery(t *testing.T) {
	r, dir := fixtureRoot(t)
	file := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(file, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	var release func()
	attempts, recoveries := 0, 0
	w := Writer{Root: r, Guard: func(context.Context) error {
		attempts++
		if attempts == 1 {
			release = lockWindowsStagedSource(t, dir)
		}
		if attempts == 2 {
			release()
		}
		return nil
	}, RecoveryReady: func(Recovery) error { recoveries++; return nil }}
	old := fileEntry("old")
	before, err := w.Replace(t.Context(), "locked.txt", &old, fileEntry("new"), bytes.NewBufferString("new"))
	if err != nil || attempts != 2 || recoveries != 1 {
		t.Fatal("transient sharing did not complete exactly one replacement", err, attempts, recoveries)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "new" {
		t.Fatal("replacement bytes", string(data), err)
	}
	assertWindowsRecovery(t, dir, before)
}

func assertWindowsRecovery(t *testing.T, directory string, recovery Recovery) {
	t.Helper()
	if recovery.Path == "" {
		t.Fatal("missing recovery reference")
	}
	if data, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(recovery.Path))); err != nil || string(data) != "old" {
		t.Fatal("original recovery bytes lost", string(data), err)
	}
	if matches, err := filepath.Glob(filepath.Join(directory, ".agentbox-sync-tmp-*")); err != nil || len(matches) != 0 {
		t.Fatal("staging file leaked", matches, err)
	}
}

func TestWindowsRetryRechecksTargetLeaseCancellationAndStage(t *testing.T) {
	for _, mutation := range []string{"target", "lease", "cancel", "stage", "stage_in_place"} {
		t.Run(mutation, func(t *testing.T) {
			r, dir := fixtureRoot(t)
			file := filepath.Join(dir, "locked.txt")
			if err := os.WriteFile(file, []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			var release func()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			attempts := 0
			w := Writer{Root: r, Guard: func(context.Context) error {
				attempts++
				if attempts == 1 {
					release = lockWindowsStagedSource(t, dir)
				}
				if attempts != 2 {
					return nil
				}
				release()
				switch mutation {
				case "target":
					return os.WriteFile(file, []byte("editor"), 0644)
				case "lease":
					return syncproto.ErrLeaseExpired
				case "cancel":
					cancel()
				case "stage", "stage_in_place":
					matches, err := filepath.Glob(filepath.Join(dir, ".agentbox-sync-tmp-*"))
					if err != nil || len(matches) != 1 {
						t.Fatal("missing staged file", matches, err)
					}
					if mutation == "stage_in_place" {
						return os.WriteFile(matches[0], []byte("bad"), 0644)
					}
					if err := os.Rename(matches[0], filepath.Join(dir, "held-original-stage")); err != nil {
						return err
					}
					return os.WriteFile(matches[0], []byte("other"), 0644)
				}
				return nil
			}}
			old := fileEntry("old")
			before, err := w.Replace(ctx, "locked.txt", &old, fileEntry("new"), bytes.NewBufferString("new"))
			want := syncproto.ErrChanged
			if mutation == "lease" {
				want = syncproto.ErrLeaseExpired
			}
			if mutation == "cancel" {
				want = context.Canceled
			}
			if attempts != 2 || !errors.Is(err, want) {
				t.Fatal("changed retry was not refused", attempts, err)
			}
			expected := "old"
			if mutation == "target" {
				expected = "editor"
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != expected {
				t.Fatal("target overwritten after change", string(data), err)
			}
			assertWindowsRecovery(t, dir, before)
		})
	}
}

func TestWindowsLockedDestinationKeepsOriginalBytes(t *testing.T) {
	r, dir := fixtureRoot(t)
	w := Writer{Root: r}
	file := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(file, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	// Permit reading for hashing/recovery but deny rename/delete while an editor
	// holds the destination. The implementation must not unlink it as fallback.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := fileEntry("old")
	new := fileEntry("new")
	attempts := 0
	w.Guard = func(context.Context) error { attempts++; return nil }
	recovery, replaceErr := w.Replace(t.Context(), "locked.txt", &old, new, bytes.NewBufferString("new"))
	windows.CloseHandle(handle)
	if replaceErr == nil {
		t.Fatal("replaced file denied by Windows sharing mode")
	}
	switch {
	case retryableReplaceError(replaceErr):
		if attempts != 6 {
			t.Fatal("persistent sharing lock was not bounded", attempts, replaceErr)
		}
	case errors.Is(replaceErr, windows.ERROR_ACCESS_DENIED):
		if attempts != 1 {
			t.Fatal("permanent target access denial was retried", attempts, replaceErr)
		}
	default:
		t.Fatal("unexpected Windows target lock error", replaceErr)
	}
	assertWindowsRecovery(t, dir, recovery)
	if data, err := os.ReadFile(file); err != nil || string(data) != "old" {
		t.Fatalf("locked target changed: %q %v", data, err)
	}
	if _, err = w.Replace(t.Context(), "locked.txt", &old, new, bytes.NewBufferString("new")); err != nil {
		t.Fatal("retry after unlock", err)
	}
}
