package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"time"
)

// SchemaVersion changes only with a committed, ordered migration. Versions
// predating this framework use user_version=0, including partially upgraded DBs.
const SchemaVersion = 3

//go:embed migrations/001_baseline.sql
var baselineSQL string

//go:embed migrations/002_usage_price.sql
var usagePriceSQL string

type migration struct {
	version int
	apply   func(*sql.Tx) error
}

func migrations() []migration {
	return []migration{
		{1, baselineMigration},
		{2, func(tx *sql.Tx) error { _, err := tx.Exec(usagePriceSQL); return err }},
		{3, normalizeUsageTimestamps},
	}
}
func migrate(db *sql.DB) error { return runMigrations(db, migrations()) }
func runMigrations(db *sql.DB, steps []migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	latest := steps[len(steps)-1].version
	if version < 0 || version > latest {
		return fmt.Errorf("unsupported database schema version %d (maximum %d); use a compatible binary or restore a compatible backup", version, latest)
	}
	for i, step := range steps {
		if step.version != i+1 {
			return fmt.Errorf("non-contiguous migration version %d", step.version)
		}
		if step.version <= version {
			continue
		}
		if err := step.apply(tx); err != nil {
			return fmt.Errorf("migration %d: %w", step.version, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.version)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func baselineMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(baselineSQL); err != nil {
		return err
	}
	// Bootstrap pre-versioned installations, including those stopped halfway
	// through an old additive migration. Inspect schema; never parse error text.
	for _, c := range []struct{ table, name, definition string }{
		{"sessions", "default_model", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "stop_reason", "TEXT NOT NULL DEFAULT ''"},
		{"usage_events", "kind", "TEXT NOT NULL DEFAULT 'chat'"},
		{"usage_events", "ttft_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_events", "provider", "TEXT NOT NULL DEFAULT ''"},
		{"usage_events", "wall_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"usage_events", "req_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		var found int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", c.table, c.name).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			if _, err := tx.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.name + " " + c.definition); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_req ON usage_events(req_id) WHERE req_id != '';
 CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_events(ts);`)
	return err
}

func normalizeUsageTimestamps(tx *sql.Tx) error {
	var lastID int64
	for {
		rows, err := tx.Query("SELECT id, ts FROM usage_events WHERE id > ? ORDER BY id LIMIT 1000", lastID)
		if err != nil {
			return err
		}
		type entry struct {
			id int64
			ts string
		}
		var batch []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.id, &e.ts); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, e := range batch {
			ts, err := time.Parse(time.RFC3339Nano, e.ts)
			if err != nil {
				return fmt.Errorf("usage event %d timestamp %q: %w", e.id, e.ts, err)
			}
			if _, err := tx.Exec("UPDATE usage_events SET ts = ? WHERE id = ?", usageTimestamp(ts), e.id); err != nil {
				return err
			}
		}
		lastID = batch[len(batch)-1].id
	}
}
