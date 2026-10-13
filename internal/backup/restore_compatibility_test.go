package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These deliberately small old databases exercise the backup format's promise
// to preserve an existing schema without bootstrapping any service tables.
func restoreCompatibilityFixture(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(base, "config.json")
	if err := os.WriteFile(config, []byte(`{"data_dir":"data","accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(base, "data/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, query := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE historical_data(value TEXT NOT NULL)",
		"INSERT INTO historical_data VALUES('committed in original WAL')",
		fmt.Sprintf("PRAGMA user_version=%d", version),
	} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err = Create(t.Context(), Options{Config: config, Output: archive}); err != nil {
		t.Fatal(err)
	}
	return archive, db
}

func restoreSchema(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var schema []string
	for rows.Next() {
		var statement string
		if err = rows.Scan(&statement); err != nil {
			t.Fatal(err)
		}
		schema = append(schema, statement)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return schema
}

func assertStandaloneRestoredDB(t *testing.T, file string) {
	t.Helper()
	db, err := openDB(file)
	if err != nil {
		t.Fatal(err)
	}
	var journal string
	if err = db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if journal != "delete" {
		t.Fatalf("restore changed standalone snapshot journal to %q", journal)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err = os.Stat(file + suffix); !os.IsNotExist(err) {
			t.Fatalf("restore retained SQLite sidecar %s: %v", suffix, err)
		}
	}
}

func TestRestorePreservesHistoricalSchemaAndJournal(t *testing.T) {
	for _, version := range []int{0, 12} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			archive, source := restoreCompatibilityFixture(t, version)
			wantSchema := restoreSchema(t, source)
			target := filepath.Join(t.TempDir(), "restored")
			if _, err := Restore(t.Context(), archive, target); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(target, "data/state.db")
			assertStandaloneRestoredDB(t, file)
			restored, err := openDB(file)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if got := restoreSchema(t, restored); !reflect.DeepEqual(got, wantSchema) {
				t.Fatalf("restore migrated historical schema: got %v, want %v", got, wantSchema)
			}
			var gotVersion int
			if err = restored.QueryRow("PRAGMA user_version").Scan(&gotVersion); err != nil || gotVersion != version {
				t.Fatalf("restore changed schema version: got %d, want %d: %v", gotVersion, version, err)
			}
			var value, journal string
			if err = restored.QueryRow("SELECT value FROM historical_data").Scan(&value); err != nil || value != "committed in original WAL" {
				t.Fatal("historical WAL data lost", value, err)
			}
			if err = source.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
				t.Fatal("source journal changed", journal, err)
			}
		})
	}
}
