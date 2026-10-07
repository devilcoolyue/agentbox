package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMaintenanceNeverCreatesMigratesOrImports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if s, err := OpenForMaintenance(path); err == nil {
		s.Close()
		t.Fatal("created a missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing database was created", err)
	}
	for _, version := range []int{0, SchemaVersion - 1, SchemaVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d; CREATE TABLE sentinel(value TEXT); INSERT INTO sentinel VALUES ('keep')", version)); err != nil {
				t.Fatal(err)
			}
			if s, err := OpenForMaintenance(path); err == nil {
				s.Close()
				t.Fatal("accepted mismatched schema")
			}
			var after, tables int
			var mode, sentinel string
			if err := db.QueryRow("PRAGMA user_version").Scan(&after); err != nil || after != version {
				t.Fatal("schema changed", err)
			}
			if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
				t.Fatal("journal mode changed", err)
			}
			if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil || tables != 1 {
				t.Fatal("tables changed", err)
			}
			if err := db.QueryRow("SELECT value FROM sentinel").Scan(&sentinel); err != nil || sentinel != "keep" {
				t.Fatal("data changed", err)
			}
		})
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	legacy := filepath.Join(filepath.Dir(path), "state.json")
	if err := os.WriteFile(legacy, []byte("not JSON: maintenance must not import this"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = OpenForMaintenance(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var synchronous int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatal("maintenance must use FULL durability", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("legacy file changed", err)
	}
}
