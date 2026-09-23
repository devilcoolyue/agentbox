package store

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFutureSchemaRejectedWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE future_only(value TEXT); INSERT INTO future_only VALUES ('keep'); PRAGMA user_version=999;"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := Open(path); err == nil {
		st.Close()
		t.Fatal("future schema accepted")
	} else if !strings.Contains(err.Error(), "999") {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected database modified")
	}
}
func TestMigrationFailureRollsBackSchemaAndVersion(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = runMigrations(db, []migration{{1, func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE partial(value TEXT)"); return err }}, {2, func(tx *sql.Tx) error { _, err := tx.Exec("INSERT INTO missing_table VALUES (1)"); return err }}})
	if err == nil {
		t.Fatal("failed migration accepted")
	}
	var version, tables int
	db.QueryRow("PRAGMA user_version").Scan(&version)
	db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='partial'").Scan(&tables)
	if version != 0 || tables != 0 {
		t.Fatalf("partial migration persisted: version %d tables %d", version, tables)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal("repeated migration", err)
	}
	db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion {
		t.Fatal(version)
	}
}
func TestPreVersionedCurrentDatabaseKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(Session{ID: "legacy", User: "alice", Name: "keep", DefaultModel: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("ALTER TABLE usage_events DROP COLUMN price_snapshot; PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, ok := st.Get("legacy")
	if !ok || row.DefaultModel != "fixture" {
		t.Fatalf("bootstrap lost row %+v", row)
	}
}

func TestVersionOneUpgradeAndDataPreservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db, []migration{{1, baselineMigration}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_events(ts,user,session_id,turn_id,agent,model,cost_micro_usd) VALUES ('2026-09-23T00:00:00Z','alice','s1','t1','claude','fixture',42)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows := st.ListUsage(UsageFilter{User: "alice"})
	if len(rows) != 1 || rows[0].CostMicroUSD != 42 || rows[0].Price != nil {
		t.Fatalf("upgrade rewrote historical rows: %+v", rows)
	}
	var version int
	st.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion {
		t.Fatal(version)
	}
}

func TestMigrationInterruptedProcessLeavesNoPartialSchema(t *testing.T) {
	if path := os.Getenv("AGENTBOX_TEST_MIGRATION_CRASH"); path != "" {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			os.Exit(12)
		}
		_ = runMigrations(db, []migration{{1, func(tx *sql.Tx) error {
			if _, err := tx.Exec("CREATE TABLE interrupted(value TEXT); PRAGMA user_version=1"); err != nil {
				os.Exit(13)
			}
			os.Exit(23) // abrupt process exit before transaction commit
			return nil
		}}})
		os.Exit(14)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMigrationInterruptedProcessLeavesNoPartialSchema$")
	cmd.Env = append(os.Environ(), "AGENTBOX_TEST_MIGRATION_CRASH="+path)
	err := cmd.Run()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 23 {
		t.Fatalf("crash fixture failed: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interrupted'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if version != 0 || tables != 0 {
		t.Fatalf("interrupted transaction leaked schema: %d %d", version, tables)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}
