//go:build linux

package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"agentbox/internal/syncclient"
	"agentbox/internal/syncfs"
)

// These tests require a separate, initially empty <=8 MiB tmpfs, never the
// host's ordinary temp/project disk. Build this test binary first, then run it
// in a read-only-root Linux container as root (the real server fixture chowns
// its workspace). No product failure injection or SQLite max_page_count is used.
func TestSyncEngineRealENOSPC(t *testing.T) {
	if os.Getenv("AGENTBOX_ENGINE_FULL_TEST") != "1" {
		t.Skip("set AGENTBOX_ENGINE_FULL_TEST=1 and provide an isolated small tmpfs")
	}
	volume := requireEngineFullVolume(t)
	for _, stage := range []string{"prepared", "started"} {
		t.Run("state_"+stage, func(t *testing.T) { engineStateENOSPC(t, volume, stage) })
	}
	t.Run("download_project", func(t *testing.T) { engineDownloadENOSPC(t, volume) })
}

type engineFullVolume struct {
	base string
	fsid syscall.Fsid
}

func requireEngineFullVolume(t *testing.T) engineFullVolume {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("the isolated Linux server fixture requires root for workspace ownership")
	}
	base := os.Getenv("AGENTBOX_ENGINE_FULL_TEST_ROOT")
	if !filepath.IsAbs(base) || filepath.Clean(base) != base || base == "/" {
		t.Fatal("explicit isolated tmpfs mount path is required")
	}
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil || resolved != base {
		t.Fatal("fault volume must be a real directory without symlink aliases")
	}
	var mount, parent syscall.Statfs_t
	if syscall.Statfs(base, &mount) != nil || syscall.Statfs(filepath.Dir(base), &parent) != nil {
		t.Fatal("cannot verify the fault volume")
	}
	if mount.Type != 0x01021994 || mount.Bsize <= 0 || mount.Blocks == 0 || mount.Blocks > (8<<20)/uint64(mount.Bsize) || mount.Fsid == parent.Fsid {
		t.Fatal("fault volume must be a dedicated tmpfs mount of at most 8 MiB")
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatal("refusing to fill a nonempty or unreadable fault volume")
	}
	t.Logf("validated isolated tmpfs capacity=%d bytes", mount.Blocks*uint64(mount.Bsize))
	return engineFullVolume{base: base, fsid: mount.Fsid}
}

func (v engineFullVolume) directory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(v.base, "engine-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	return directory
}

// Write real non-sparse bytes until the kernel returns ENOSPC, with both mount
// identity and an independent maximum-write bound checked before touching it.
func (v engineFullVolume) fill(directory string) (string, int64, error) {
	var current syscall.Statfs_t
	if err := syscall.Statfs(directory, &current); err != nil {
		return "", 0, err
	}
	if current.Type != 0x01021994 || current.Fsid != v.fsid || current.Bsize <= 0 || current.Blocks > (8<<20)/uint64(current.Bsize) {
		return "", 0, errors.New("fault volume changed before fill")
	}
	path := filepath.Join(directory, "allocated-fill-bytes")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", 0, err
	}
	block := make([]byte, 64<<10)
	for i := range block {
		block[i] = byte(i%251 + 1)
	}
	var written int64
	for attempts := 0; attempts <= (8<<20)/len(block); attempts++ {
		n, writeErr := file.Write(block)
		written += int64(n)
		if writeErr != nil {
			closeErr := file.Close()
			if !errors.Is(writeErr, syscall.ENOSPC) {
				return path, written, writeErr
			}
			if closeErr != nil {
				return path, written, closeErr
			}
			if err := syscall.Statfs(directory, &current); err != nil {
				return path, written, err
			}
			if current.Fsid != v.fsid || current.Bavail != 0 {
				return path, written, errors.New("ENOSPC did not exhaust the approved tmpfs")
			}
			return path, written, nil
		}
	}
	file.Close()
	return path, written, errors.New("bounded fill never reached real ENOSPC")
}

func relocateFullEngine(t *testing.T, f *engineFixture, local, stateDir string) {
	t.Helper()
	if err := f.engine.State.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(local, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := syncfs.OpenContext(t.Context(), local)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := root.Identity()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	state, err := syncclient.OpenStateContext(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	binding := f.saved.Binding
	binding.LocalID = identity
	saved, err := state.RegisterContext(t.Context(), binding, local)
	if err != nil {
		t.Fatal(err)
	}
	f.engine.State, f.saved, f.local, f.stateDir = state, saved, local, stateDir
}

// Use a separate read-only SQLite connection, not the executor's page cache,
// to verify committed state and the entire database after failed disk writes.
func readFullEngineState(f *engineFixture) (syncclient.SavedBinding, error) {
	var saved syncclient.SavedBinding
	uri := (&url.URL{Scheme: "file", Path: filepath.Join(f.stateDir, "sync.db")}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro")
	if err != nil {
		return saved, err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return saved, err
	}
	if integrity != "ok" {
		return saved, errors.New("SQLite integrity check failed")
	}
	var raw []byte
	if err := db.QueryRow("SELECT state FROM bindings WHERE id=?", f.saved.ID).Scan(&raw); err != nil {
		return saved, err
	}
	err = json.Unmarshal(raw, &saved)
	return saved, err
}

func fullEngineState(t *testing.T, f *engineFixture) syncclient.SavedBinding {
	t.Helper()
	saved, err := readFullEngineState(f)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func fullEngineBytes(t *testing.T, root, expected string) {
	t.Helper()
	actual, err := os.ReadFile(filepath.Join(root, "file.bin"))
	if err != nil || string(actual) != expected {
		t.Fatal("file content changed at an uncommitted boundary", err)
	}
}

func engineStateENOSPC(t *testing.T, volume engineFullVolume, stage string) {
	directory := volume.directory(t)
	var applyCalls atomic.Int32
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/apply") {
				applyCalls.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	relocateFullEngine(t, f, f.local, filepath.Join(directory, "state"))
	const original = "committed baseline\r\n\x00"
	const desired = "local edit to upload\r\n\x00"
	writeEngineFile(t, f.local, "file.bin", original)
	writeEngineFile(t, f.remote, "file.bin", original)
	baseline := engineRound(t, f)
	writeEngineFile(t, f.local, "file.bin", desired)
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil || len(preview.Plan.Operations) != 1 || preview.Plan.Operations[0].Kind != "upload" {
		t.Fatal("upload preview", err)
	}
	var filler string
	var written int64
	beforeFailure := baseline
	work := t.Context()
	if stage == "prepared" {
		filler, written, err = volume.fill(directory)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		// Progress only coordinates the physical fault timing. It returns no
		// artificial failure and does not alter any SQLite or engine setting.
		work = syncclient.WithProgress(work, func(progress syncclient.Progress) {
			if progress.Stage != "applying" || progress.Operation != "upload" || filler != "" {
				return
			}
			beforeFailure = fullEngineState(t, f)
			if beforeFailure.Pending == nil || beforeFailure.Pending.Operations[0].Status != "prepared" {
				t.Fatal("did not reach the committed prepared boundary")
			}
			filler, written, err = volume.fill(directory)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	_, err = f.engine.Apply(work, f.saved.ID, preview, preview.Plan.Digest)
	var sqliteError interface{ Code() int }
	if filler == "" || err == nil || !errors.As(err, &sqliteError) || sqliteError.Code()&0xff != 13 {
		t.Fatalf("expected real SQLite FULL after kernel ENOSPC at %s, got %v", stage, err)
	}
	failed := fullEngineState(t, f)
	if !reflect.DeepEqual(failed, beforeFailure) || !reflect.DeepEqual(failed.Baseline, baseline.Baseline) || applyCalls.Load() != 0 {
		t.Fatal("failed durable intent changed committed state or sent a remote mutation")
	}
	fullEngineBytes(t, f.local, desired)
	fullEngineBytes(t, f.remote, original)
	if f.server.syncLeases().Active(f.session.ID, f.project.ID) {
		t.Fatal("failed SQLite intent leaked its lease")
	}
	t.Logf("%s persistence: wrote %d bytes to kernel ENOSPC, SQLite code=%d, HTTP apply=0, integrity/baseline/files preserved", stage, written, sqliteError.Code())
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	if failed.Pending != nil {
		if failed.Pending.Operations[0].Status != "prepared" {
			t.Fatal("failed started persistence became an acknowledged attempt")
		}
		if _, err := f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); !errors.Is(err, syncclient.ErrPending) {
			t.Fatal("pending intent was blindly retried", err)
		}
		review, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
		if err != nil || review.CanFinish || review.Items[0].Recorded != "prepared" || review.Items[0].Receipt != "not_attempted" {
			t.Fatal("prepared recovery review", err, review)
		}
		if _, err := f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "replan"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fullEngineState(t, f).Baseline, baseline.Baseline) {
			t.Fatal("replan advanced the baseline")
		}
	}
	finished := engineRound(t, f)
	if finished.Pending != nil || applyCalls.Load() != 1 {
		t.Fatal("free-space retry did not perform exactly one confirmed upload")
	}
	fullEngineBytes(t, f.local, desired)
	fullEngineBytes(t, f.remote, desired)
	_ = fullEngineState(t, f)
}

func engineDownloadENOSPC(t *testing.T, volume engineFullVolume) {
	directory := volume.directory(t)
	project := filepath.Join(directory, "project")
	type fault struct {
		filler  string
		bytes   int64
		started syncclient.SavedBinding
		err     error
	}
	faults := make(chan fault, 1)
	var armed atomic.Bool
	var downloads, applies atomic.Int32
	var f *engineFixture
	f = newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/apply") {
				applies.Add(1)
			}
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sync/file") {
				downloads.Add(1)
				if armed.Swap(false) {
					var observed fault
					observed.started, observed.err = readFullEngineState(f)
					if observed.err == nil && (observed.started.Pending == nil || observed.started.Pending.Operations[0].Status != "started") {
						observed.err = errors.New("HTTP download preceded committed started intent")
					}
					if observed.err == nil {
						observed.filler, observed.bytes, observed.err = volume.fill(directory)
					}
					faults <- observed
					if observed.err != nil {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	relocateFullEngine(t, f, project, filepath.Join(t.TempDir(), "state"))
	const original = "local original survives\r\n\x00"
	desired := strings.Repeat("remote-download\r\n\x00", 4096)
	writeEngineFile(t, f.local, "file.bin", original)
	writeEngineFile(t, f.remote, "file.bin", original)
	baseline := engineRound(t, f)
	writeEngineFile(t, f.remote, "file.bin", desired)
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil || len(preview.Plan.Operations) != 1 || preview.Plan.Operations[0].Kind != "download" {
		t.Fatal("download preview", err)
	}
	armed.Store(true)
	var received int64
	work := syncclient.WithProgress(t.Context(), func(progress syncclient.Progress) {
		if progress.Bytes > received {
			received = progress.Bytes
		}
	})
	_, err = f.engine.Apply(work, f.saved.ID, preview, preview.Plan.Digest)
	var observed fault
	select {
	case observed = <-faults:
	default:
		t.Fatal("real HTTP request never reached the fault window", err)
	}
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	if !errors.Is(err, syscall.ENOSPC) || received == 0 || downloads.Load() != 1 || applies.Load() != 0 {
		t.Fatalf("expected actual HTTP bytes followed by local ENOSPC: err=%v received=%d GETs=%d applies=%d", err, received, downloads.Load(), applies.Load())
	}
	failed := fullEngineState(t, f)
	if !reflect.DeepEqual(failed, observed.started) || !reflect.DeepEqual(failed.Baseline, baseline.Baseline) || failed.Pending.Operations[0].Status != "started" {
		t.Fatal("download disk-full lost pending or advanced baseline")
	}
	fullEngineBytes(t, f.local, original)
	fullEngineBytes(t, f.remote, desired)
	t.Logf("download: committed started, filled %d bytes to kernel ENOSPC, client read %d HTTP bytes, old file and baseline preserved", observed.bytes, received)
	if err := os.Remove(observed.filler); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("download blindly replayed after space returned", err)
	}
	review, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil || review.CanFinish || len(review.Items) != 1 || review.Items[0].Recorded != "started" || review.Items[0].Current != "before" || review.Items[0].Receipt != "local" {
		t.Fatal("download recovery review", err, review)
	}
	if _, err := f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "finish"); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("unfinished download was committed", err)
	}
	if _, err := f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "replan"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fullEngineState(t, f).Baseline, baseline.Baseline) {
		t.Fatal("download replan changed baseline")
	}
	finished := engineRound(t, f)
	if finished.Pending != nil || downloads.Load() != 2 || applies.Load() != 0 {
		t.Fatal("download retry did not converge without remote mutation")
	}
	fullEngineBytes(t, f.local, desired)
	fullEngineBytes(t, f.remote, desired)
	history, err := f.engine.RecoveryHistory(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	exported := false
	for _, batch := range history {
		for _, file := range batch.Files {
			if file.Side != "local" {
				continue
			}
			destination := t.TempDir()
			name, err := f.engine.ExportRecovery(t.Context(), f.saved.ID, batch.ID, file.ID, destination)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(destination, name))
			if err != nil || string(data) != original {
				t.Fatal("successful retry lost the original recovery bytes", err)
			}
			exported = true
		}
	}
	if !exported {
		t.Fatal("successful retry did not retain an exportable original")
	}
	_ = fullEngineState(t, f)
	t.Log("released space, reviewed/replanned, downloaded successfully, and exported the byte-exact original")
}
