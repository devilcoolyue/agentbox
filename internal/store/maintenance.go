package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
)

// OpenForMaintenance opens an existing, current-schema database without
// creating, migrating, importing legacy JSON or changing its journal mode.
// The caller must hold the instance data-directory lock until Close completes.
func OpenForMaintenance(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	db, err := sql.Open("sqlite", u.String()+"?mode=rw&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err == nil && version != SchemaVersion {
		err = fmt.Errorf("database schema %d does not match binary schema %d; use the matching installed binary", version, SchemaVersion)
	}
	if err == nil {
		_, err = db.Exec("PRAGMA synchronous=FULL")
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}
