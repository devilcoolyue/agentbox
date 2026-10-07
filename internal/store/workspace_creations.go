package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrCreationConflict = errors.New("creation request conflicts with an existing receipt")
var ErrCreationGone = errors.New("creation workspace was deleted or abandoned")
var ErrImportPending = errors.New("project import is running or needs review")

type WorkspaceCreation struct {
	RequestID       string          `json:"request_id"`
	Fingerprint     string          `json:"-"`
	Request         json.RawMessage `json:"request"`
	Session         Session         `json:"session"`
	GitConnectionID string          `json:"git_connection_id"`
	State           string          `json:"state"`
	CreatedAt       string          `json:"created_at"`
}

func ownerEpoch(u User) string { return u.CreatedAt.Format(time.RFC3339Nano) }
func checkCreationOwner(tx *sql.Tx, u User) error {
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM users WHERE name=? AND created_at=?", u.Name, ownerEpoch(u)).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

const creationColumns = "request_id,fingerprint,request_json,session_json,git_connection_id,state,created_at"

func scanCreation(row interface{ Scan(...any) error }) (WorkspaceCreation, error) {
	var c WorkspaceCreation
	var request, session string
	err := row.Scan(&c.RequestID, &c.Fingerprint, &request, &session, &c.GitConnectionID, &c.State, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	c.Request = json.RawMessage(request)
	err = json.Unmarshal([]byte(session), &c.Session)
	return c, err
}
func (s *Store) WorkspaceCreation(u User, id string) (WorkspaceCreation, error) {
	return scanCreation(s.db.QueryRow("SELECT "+creationColumns+" FROM workspace_creations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id))
}
func (s *Store) PendingWorkspaceCreations(u User) ([]WorkspaceCreation, error) {
	rows, err := s.db.Query("SELECT "+creationColumns+" FROM workspace_creations WHERE user=? AND user_created_at=? AND state IN ('reserved','ready') ORDER BY rowid DESC LIMIT 50", u.Name, ownerEpoch(u))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkspaceCreation{}
	for rows.Next() {
		c, err := scanCreation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) ReserveWorkspaceCreation(u User, c WorkspaceCreation) (WorkspaceCreation, error) {
	if u.Name == "" || u.CreatedAt.IsZero() || c.Session.User != u.Name || c.Session.ID == "" || c.RequestID == "" || c.Fingerprint == "" || !json.Valid(c.Request) {
		return c, ErrCreationConflict
	}
	tx, err := s.db.Begin()
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return c, err
	}
	existing, err := scanCreation(tx.QueryRow("SELECT "+creationColumns+" FROM workspace_creations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), c.RequestID))
	if err == nil {
		if existing.Fingerprint != c.Fingerprint {
			return existing, ErrCreationConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM sessions WHERE id=?", c.Session.ID).Scan(&count); err != nil {
		return c, err
	}
	if count != 0 {
		return c, ErrCreationConflict
	}
	encoded, err := json.Marshal(c.Session)
	if err != nil {
		return c, err
	}
	c.State = "reserved"
	c.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`INSERT INTO workspace_creations(user,user_created_at,request_id,fingerprint,request_json,session_json,session_id,git_connection_id,state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, u.Name, ownerEpoch(u), c.RequestID, c.Fingerprint, string(c.Request), string(encoded), c.Session.ID, c.GitConnectionID, c.State, c.CreatedAt)
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}

// Publish session/default binding/receipt together. Never UPSERT over another
// workspace. Filesystem preparation precedes this transaction and is reusable.
func (s *Store) CompleteWorkspaceCreation(u User, id string) (Session, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return Session{}, err
	}
	c, err := scanCreation(tx.QueryRow("SELECT "+creationColumns+" FROM workspace_creations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id))
	if err != nil {
		return Session{}, err
	}
	if c.State != "reserved" {
		return Session{}, ErrCreationConflict
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM sessions WHERE id=?", c.Session.ID).Scan(&n); err != nil {
		return Session{}, err
	}
	if n != 0 {
		return Session{}, ErrCreationConflict
	}
	if c.GitConnectionID != "" {
		if err = tx.QueryRow(gitCanSelectSQL, c.GitConnectionID, u.Name, u.Name).Scan(&n); err != nil {
			return Session{}, err
		}
		if n != 1 {
			return Session{}, sql.ErrNoRows
		}
	}
	c.Session.UpdatedAt = time.Now()
	if err = s.put(tx, c.Session); err != nil {
		return Session{}, err
	}
	if c.GitConnectionID != "" {
		if _, err = tx.Exec("INSERT INTO git_defaults(user,session_id,connection_id) VALUES(?,?,?)", u.Name, c.Session.ID, c.GitConnectionID); err != nil {
			return Session{}, err
		}
	}
	if _, err = tx.Exec("UPDATE workspace_creations SET state='ready' WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id); err != nil {
		return Session{}, err
	}
	return c.Session, tx.Commit()
}

func (s *Store) FinishWorkspaceCreation(u User, id string, abandon bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return err
	}
	c, err := scanCreation(tx.QueryRow("SELECT "+creationColumns+" FROM workspace_creations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id))
	if err != nil {
		return err
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM workspace_imports WHERE session_id=? AND state IN ('running','uncertain')", c.Session.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrImportPending
	}
	state := "complete"
	if abandon {
		state = "abandoned"
	} else if c.State != "ready" && c.State != "complete" {
		return ErrCreationGone
	}
	_, err = tx.Exec("UPDATE workspace_creations SET state=? WHERE user=? AND user_created_at=? AND request_id=?", state, u.Name, ownerEpoch(u), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Fence a request that has not arrived yet. A delayed create with this ID must
// conflict rather than allocating a workspace after the user starts over.
func (s *Store) AbandonUnknownWorkspaceCreation(u User, id, sessionID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return err
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM workspace_creations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrCreationConflict
	}
	raw, _ := json.Marshal(Session{ID: sessionID, User: u.Name})
	_, err = tx.Exec(`INSERT INTO workspace_creations(user,user_created_at,request_id,fingerprint,request_json,session_json,session_id,git_connection_id,state,created_at) VALUES(?,?,?,'','{}',?,?,'','abandoned',?)`, u.Name, ownerEpoch(u), id, string(raw), sessionID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

type WorkspaceImport struct {
	AttemptID   string          `json:"attempt_id"`
	Kind        string          `json:"kind"`
	Directory   string          `json:"directory"`
	State       string          `json:"state"`
	Result      json.RawMessage `json:"result"`
	Fingerprint string          `json:"-"`
	CreatedAt   string          `json:"created_at"`
}

const importColumns = "attempt_id,kind,directory,state,result_json,fingerprint,created_at"

func (s *Store) WorkspaceImport(u User, id, attempt string) (WorkspaceImport, error) {
	return scanWorkspaceImport(s.db.QueryRow("SELECT "+importColumns+" FROM workspace_imports WHERE user=? AND user_created_at=? AND request_id=? AND attempt_id=?", u.Name, ownerEpoch(u), id, attempt))
}

func scanWorkspaceImport(row interface{ Scan(...any) error }) (WorkspaceImport, error) {
	var i WorkspaceImport
	var raw string
	err := row.Scan(&i.AttemptID, &i.Kind, &i.Directory, &i.State, &raw, &i.Fingerprint, &i.CreatedAt)
	i.Result = json.RawMessage(raw)
	return i, err
}
func (s *Store) WorkspaceImports(u User, id string) ([]WorkspaceImport, error) {
	rows, err := s.db.Query("SELECT "+importColumns+" FROM workspace_imports WHERE user=? AND user_created_at=? AND request_id=? ORDER BY rowid DESC LIMIT 50", u.Name, ownerEpoch(u), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []WorkspaceImport{}
	for rows.Next() {
		i, err := scanWorkspaceImport(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}

func (s *Store) BeginWorkspaceImport(u User, id string, i WorkspaceImport) (WorkspaceImport, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return i, false, err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return i, false, err
	}
	c, err := scanCreation(tx.QueryRow("SELECT "+creationColumns+" FROM workspace_creations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id))
	if err != nil {
		return i, false, err
	}
	if c.State != "ready" {
		return i, false, ErrCreationGone
	}
	old, err := scanWorkspaceImport(tx.QueryRow("SELECT "+importColumns+" FROM workspace_imports WHERE user=? AND user_created_at=? AND request_id=? AND attempt_id=?", u.Name, ownerEpoch(u), id, i.AttemptID))
	if err == nil {
		if old.Fingerprint != i.Fingerprint {
			return old, false, ErrCreationConflict
		}
		return old, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return i, false, err
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM workspace_imports WHERE session_id=? AND state IN ('running','uncertain')", c.Session.ID).Scan(&n); err != nil {
		return i, false, err
	}
	if n > 0 {
		return i, false, ErrImportPending
	}
	i.State = "running"
	i.Result = json.RawMessage(`{}`)
	i.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`INSERT INTO workspace_imports(user,user_created_at,request_id,attempt_id,session_id,fingerprint,kind,directory,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, u.Name, ownerEpoch(u), id, i.AttemptID, c.Session.ID, i.Fingerprint, i.Kind, i.Directory, i.State, i.CreatedAt, i.CreatedAt)
	if err != nil {
		return i, false, err
	}
	return i, true, tx.Commit()
}
func (s *Store) FinishWorkspaceImport(u User, id, attempt, state string, result json.RawMessage) error {
	if state != "succeeded" && state != "failed" && state != "uncertain" {
		return ErrCreationConflict
	}
	res, err := s.db.Exec("UPDATE workspace_imports SET state=?,result_json=?,updated_at=? WHERE user=? AND user_created_at=? AND request_id=? AND attempt_id=? AND state='running'", state, string(result), time.Now().UTC().Format(time.RFC3339Nano), u.Name, ownerEpoch(u), id, attempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrCreationConflict
	}
	return err
}
func (s *Store) ReviewWorkspaceImport(u User, id, attempt string) error {
	res, err := s.db.Exec("UPDATE workspace_imports SET state='reviewed',updated_at=? WHERE user=? AND user_created_at=? AND request_id=? AND attempt_id=? AND state IN ('running','uncertain')", time.Now().UTC().Format(time.RFC3339Nano), u.Name, ownerEpoch(u), id, attempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrCreationConflict
	}
	return err
}
func (s *Store) RecoverWorkspaceImports() error {
	_, err := s.db.Exec("UPDATE workspace_imports SET state='uncertain' WHERE state='running'")
	return err
}
