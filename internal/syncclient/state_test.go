package syncclient

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

func stateFixture(t *testing.T) (*StateStore, SavedBinding, string) {
	t.Helper()
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := syncfs.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := root.Probe()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	private = filepath.Join(private, "sync")
	store, err := OpenState(private)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	binding := planBinding()
	binding.LocalID = caps.DirectoryID
	saved, err := store.Register(binding, directory)
	if err != nil {
		t.Fatal(err)
	}
	return store, saved, private
}
func beginFixture(t *testing.T, s *StateStore, saved SavedBinding) SavedBinding {
	t.Helper()
	local, remote := tree(map[string]string{"a": "local", "b": "local"}), tree(nil)
	options := PlanOptions{LocalExecutable: true}
	preview, err := BuildPlan(saved.Binding, saved.Baseline, local, remote, options)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Begin(saved.ID, saved.Revision, 3, preview, preview.Digest, local, remote, options)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func TestStateDurableBatchAndBaselineCommit(t *testing.T) {
	s, saved, private := stateFixture(t)
	device, err := s.Device()
	if err != nil {
		t.Fatal(err)
	}
	started := beginFixture(t, s, saved)
	if started.Baseline != nil || len(started.Pending.Operations) != 2 {
		t.Fatal(started)
	}
	batchID := started.Pending.ID
	reopened, err := OpenState(private)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	mappings, err := reopened.Bindings(saved.Binding.ServerID, saved.Binding.User)
	if err != nil || len(mappings) != 1 || mappings[0].Pending == nil || mappings[0].Pending.ID != batchID {
		t.Fatal("restart cannot discover pending binding", err)
	}
	foreign, err := reopened.Bindings(saved.Binding.ServerID, "other-user")
	if err != nil || len(foreign) != 0 {
		t.Fatal("bindings leaked across user", err)
	}
	otherDevice, err := reopened.Device()
	if err != nil || otherDevice != device {
		t.Fatal("unstable device", err)
	}
	next, err := reopened.Load(saved.ID)
	if err != nil || next.Pending.ID != batchID {
		t.Fatal("lost pending batch", err)
	}
	if _, err = reopened.Commit(next.ID, next.Revision, next.Pending.Local, next.Pending.Local); !errors.Is(err, ErrPending) {
		t.Fatal("partial batch committed", err)
	}
	if _, err = reopened.StartOperation(next.ID, next.Revision, next.Pending.Operations[1].ID); !errors.Is(err, ErrPending) {
		t.Fatal("out of order publication", err)
	}
	for _, op := range next.Pending.Operations {
		next, err = reopened.StartOperation(next.ID, next.Revision, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		next, err = reopened.VerifyOperation(next.ID, next.Revision, op.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = reopened.Commit(next.ID, next.Revision, tree(nil), tree(nil)); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("wrong final trees committed", err)
	}
	final, err := reopened.Commit(next.ID, next.Revision, next.Pending.Local, next.Pending.Local)
	if err != nil {
		t.Fatal(err)
	}
	if final.Pending != nil || final.Baseline == nil {
		t.Fatal("baseline not committed")
	}
	history, err := reopened.History(final.ID, batchID)
	if err != nil || history.Operations[0].ID != started.Pending.Operations[0].ID {
		t.Fatal("operation IDs lost on commit", err)
	}
	loaded, err := s.Load(final.ID)
	if err != nil || loaded.Baseline == nil || loaded.Pending != nil {
		t.Fatal("commit not persisted", err)
	}
	plan, err := BuildPlan(loaded.Binding, loaded.Baseline, loaded.Baseline.Local, loaded.Baseline.Remote, PlanOptions{LocalExecutable: true})
	if err != nil || len(plan.Operations) != 0 || len(plan.Conflicts) != 0 {
		t.Fatal("committed baseline does not converge", plan, err)
	}
}
func TestStateConcurrentBeginAndStaleRevision(t *testing.T) {
	s, saved, private := stateFixture(t)
	other, err := OpenState(private)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	local, remote := tree(map[string]string{"a": "x"}), tree(nil)
	options := PlanOptions{LocalExecutable: true}
	plan, err := BuildPlan(saved.Binding, nil, local, remote, options)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for _, store := range []*StateStore{s, other} {
		workers.Go(func() {
			_, err := store.Begin(saved.ID, saved.Revision, 1, plan, plan.Digest, local, remote, options)
			results <- err
		})
	}
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrStateChanged) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("multiple batches acquired binding", successes)
	}
	loaded, err := s.Load(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Begin(loaded.ID, loaded.Revision, 1, plan, plan.Digest, local, remote, options); !errors.Is(err, ErrPending) {
		t.Fatal("pending batch overwritten", err)
	}
}
func TestStateRejectsChangedPreviewAndOverlappingMappings(t *testing.T) {
	s, saved, _ := stateFixture(t)
	local, remote := tree(map[string]string{"a": "x"}), tree(nil)
	opts := PlanOptions{LocalExecutable: true}
	plan, _ := BuildPlan(saved.Binding, nil, local, remote, opts)
	if _, err := s.Begin(saved.ID, saved.Revision, 1, plan, "", local, remote, opts); err == nil {
		t.Fatal("missing confirmation accepted")
	}
	if _, err := s.Begin(saved.ID, saved.Revision, 1, plan, plan.Digest, tree(map[string]string{"a": "new"}), remote, opts); !errors.Is(err, ErrStateChanged) {
		t.Fatal("changed tree accepted", err)
	}
	binding := saved.Binding
	binding.Project = "different"
	if _, err := s.Register(binding, saved.Directory); !errors.Is(err, ErrBinding) {
		t.Fatal("double local mapping accepted", err)
	}
	sub := filepath.Join(saved.Directory, "child")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := syncfs.Open(sub)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := root.Probe()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	binding.LocalID = caps.DirectoryID
	if _, err = s.Register(binding, sub); !errors.Is(err, ErrBinding) {
		t.Fatal("overlapping local mapping accepted", err)
	}
	// Even another local tree cannot simultaneously map an overlapping remote root.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err = syncfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	caps, err = root.Probe()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	binding.LocalID = caps.DirectoryID
	binding.ProjectPath = "child"
	if _, err = s.Register(binding, dir); !errors.Is(err, ErrBinding) {
		t.Fatal("overlapping remote mapping accepted", err)
	}
	binding.ServerID = syncproto.HashBytes([]byte("other server"))
	if _, err = s.Register(binding, dir); !errors.Is(err, ErrBinding) {
		t.Fatal("replacement at same URL bypassed existing binding", err)
	}
	binding.Server = "https://independent.example/"
	if _, err = s.Register(binding, dir); err != nil {
		t.Fatal("independent server mapping refused", err)
	}
}
func TestStateCorruptionDoesNotBecomeEmptyBaseline(t *testing.T) {
	s, saved, _ := stateFixture(t)
	if _, err := s.db.Exec("UPDATE bindings SET state=? WHERE id=?", []byte(`{"baseline":null}`), saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(saved.ID); err == nil {
		t.Fatal("corruption became new binding")
	}
}
func TestStateCrashPreservesIntentAndRollsBackPartialCommit(t *testing.T) {
	if private := os.Getenv("AGENTBOX_STATE_CRASH_DIR"); private != "" {
		s, err := OpenState(private)
		if err != nil {
			os.Exit(10)
		}
		saved, err := s.Load(os.Getenv("AGENTBOX_STATE_CRASH_BINDING"))
		if err != nil {
			os.Exit(11)
		}
		if os.Getenv("AGENTBOX_STATE_CRASH_KIND") == "started" {
			if _, err = s.StartOperation(saved.ID, saved.Revision, saved.Pending.Operations[0].ID); err != nil {
				os.Exit(12)
			}
			// Simulate publication in an external process; no executor is claimed here.
			if err = os.WriteFile(filepath.Join(saved.Directory, "published"), []byte("partial"), 0600); err != nil {
				os.Exit(13)
			}
		} else {
			tx, err := s.db.Begin()
			if err != nil {
				os.Exit(14)
			}
			saved.Pending = nil
			saved.Revision++
			raw, _ := json.Marshal(saved)
			if _, err = tx.Exec("UPDATE bindings SET state=?,revision=? WHERE id=?", raw, saved.Revision, saved.ID); err != nil {
				os.Exit(15)
			}
			// Deliberate crash before transaction commit, without deferred cleanup.
		}
		os.Exit(23)
	}
	for _, kind := range []string{"started", "transaction"} {
		t.Run(kind, func(t *testing.T) {
			s, saved, private := stateFixture(t)
			saved = beginFixture(t, s, saved)
			command := exec.Command(os.Args[0], "-test.run=^TestStateCrashPreservesIntentAndRollsBackPartialCommit$")
			command.Env = append(os.Environ(), "AGENTBOX_STATE_CRASH_DIR="+private, "AGENTBOX_STATE_CRASH_BINDING="+saved.ID, "AGENTBOX_STATE_CRASH_KIND="+kind)
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 23 {
				t.Fatalf("crash fixture: %v %s", err, output)
			}
			loaded, err := s.Load(saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Baseline != nil || loaded.Pending == nil || loaded.Pending.ID != saved.Pending.ID || loaded.Pending.Operations[0].ID != saved.Pending.Operations[0].ID {
				t.Fatal("crash lost original intent", loaded)
			}
			if kind == "started" {
				if loaded.Pending.Operations[0].Status != "started" {
					t.Fatal("publication ambiguity forgotten")
				}
				if _, err = s.StartOperation(loaded.ID, loaded.Revision, loaded.Pending.Operations[0].ID); !errors.Is(err, ErrPending) {
					t.Fatal("ambiguous operation blindly replayed", err)
				}
			} else if loaded.Pending.Operations[0].Status != "prepared" || loaded.Revision != saved.Revision {
				t.Fatal("uncommitted transaction leaked")
			}
		})
	}
}

func TestStateRefusesMissingDeviceAndFutureSchema(t *testing.T) {
	for _, kind := range []string{"missing_device", "future"} {
		t.Run(kind, func(t *testing.T) {
			s, _, private := stateFixture(t)
			statement := "DELETE FROM metadata WHERE key='device'"
			if kind == "future" {
				statement = "PRAGMA user_version=999"
			}
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if opened, err := OpenState(private); err == nil {
				opened.Close()
				t.Fatal("invalid identity/schema auto-repaired")
			}
			if kind == "missing_device" {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM metadata").Scan(&count); err != nil || count != 0 {
					t.Fatal("identity regenerated", count, err)
				}
			}
		})
	}
}

func TestRecoverySchemaMigrationPreservesDeviceBindingAndPending(t *testing.T) {
	for _, version := range []string{"1", "2", "3", "4"} {
		t.Run(version, func(t *testing.T) {
			s, saved, private := stateFixture(t)
			saved = beginFixture(t, s, saved)
			device, err := s.Device()
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(saved)
			if _, err = s.db.Exec("PRAGMA user_version=" + version); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenState(private)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			actual, err := reopened.Load(saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(actual)
			currentDevice, err := reopened.Device()
			if err != nil || currentDevice != device || string(before) != string(after) {
				t.Fatal("migration changed identity/state", err)
			}
			var schema int
			if err = reopened.db.QueryRow("PRAGMA user_version").Scan(&schema); err != nil || schema != 5 {
				t.Fatal(schema, err)
			}
		})
	}
}
func TestStateHistoryLimitPreservesBaselineAndRefusesNewBatch(t *testing.T) {
	s, saved, _ := stateFixture(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		if _, err = tx.Exec("INSERT INTO batches VALUES (?,?,?)", saved.ID, stateID(), []byte("{}")); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	m := tree(nil)
	plan, _ := BuildPlan(saved.Binding, nil, m, m, PlanOptions{})
	if _, err = s.Begin(saved.ID, saved.Revision, 1, plan, plan.Digest, m, m, PlanOptions{}); !errors.Is(err, syncproto.ErrLimit) {
		t.Fatal("retention overflow accepted", err)
	}
	current, err := s.Load(saved.ID)
	if err != nil || current.Revision != saved.Revision || current.Pending != nil {
		t.Fatal("failed begin mutated state", err)
	}
	tx, err = s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = checkStateCapacity(tx, saved.ID, maxStoredMetadata); !errors.Is(err, syncproto.ErrLimit) {
		t.Fatal("logical metadata cap ignored", err)
	}
}
func TestStateWindowsBaselineKeepsRemoteExecutable(t *testing.T) {
	s, saved, _ := stateFixture(t)
	local, remote := tree(nil), tree(map[string]string{"run": "bytes"})
	e := remote.Entries["run"]
	e.Executable = true
	remote.Entries["run"] = e
	opts := PlanOptions{LocalExecutable: false}
	plan, err := BuildPlan(saved.Binding, nil, local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Begin(saved.ID, saved.Revision, 1, plan, plan.Digest, local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	op := next.Pending.Operations[0].ID
	next, err = s.StartOperation(next.ID, next.Revision, op)
	if err != nil {
		t.Fatal(err)
	}
	next, err = s.VerifyOperation(next.ID, next.Revision, op, nil)
	if err != nil {
		t.Fatal(err)
	}
	local = tree(map[string]string{"run": "bytes"})
	next, err = s.Commit(next.ID, next.Revision, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if next.Baseline.Local.Entries["run"].Executable || !next.Baseline.Remote.Entries["run"].Executable {
		t.Fatal("platform mode metadata lost")
	}
}
