package store

import (
	"agentbox/internal/collaboration"
	"database/sql"
	"errors"
	"time"
)

type CollaborationApplication struct {
	RunID      string `json:"run_id"`
	ActionID   string `json:"action_id"`
	SessionID  string `json:"session_id"`
	SnapshotID string `json:"snapshot_id"`
	State      string `json:"state"`
}

func (s *Store) CollaborationApplication(u User, run string) (CollaborationApplication, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return CollaborationApplication{}, err
	}
	var a CollaborationApplication
	err := s.db.QueryRow("SELECT run_id,action_id,session_id,snapshot_id,state FROM collaboration_applications WHERE run_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1", run).Scan(&a.RunID, &a.ActionID, &a.SessionID, &a.SnapshotID, &a.State)
	return a, err
}
func (s *Store) CollaborationApplicationBlocked(session string) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM collaboration_applications WHERE session_id=? AND state IN ('staging','switching','uncertain')", session).Scan(&n)
	return n != 0, err
}
func (s *Store) BeginCollaborationApplication(u User, run, id, snapshot string, revision int64) (CollaborationApplication, bool, error) {
	if !collaboration.ValidID(id) || !collaboration.ValidID(snapshot) {
		return CollaborationApplication{}, false, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationApplication{}, false, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationApplication{}, false, err
	}
	var a CollaborationApplication
	err = tx.QueryRow("SELECT run_id,action_id,session_id,snapshot_id,state FROM collaboration_applications WHERE run_id=? AND action_id=?", run, id).Scan(&a.RunID, &a.ActionID, &a.SessionID, &a.SnapshotID, &a.State)
	if err == nil {
		if a.SnapshotID != snapshot || a.ActionID != id {
			return a, false, ErrChatConflict
		}
		return a, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return a, false, err
	}
	var finished int
	if err = tx.QueryRow("SELECT COUNT(*) FROM collaboration_applications WHERE run_id=? AND state='completed'", run).Scan(&finished); err != nil {
		return a, false, err
	}
	if finished != 0 {
		return a, false, ErrChatConflict
	}
	if r.State != CollabCompleted || r.Revision != revision {
		return a, false, ErrChatConflict
	}
	var current string
	if err = tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", run).Scan(&current); err != nil {
		return a, false, err
	}
	if current != snapshot {
		return a, false, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return a, false, err
	}
	a = CollaborationApplication{run, id, r.SourceSessionID, snapshot, "staging"}
	now := usageTimestamp(time.Now())
	if _, err = tx.Exec("INSERT INTO collaboration_applications(run_id,action_id,session_id,snapshot_id,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", run, id, a.SessionID, snapshot, a.State, now, now); err != nil {
		return a, false, err
	}
	return a, true, tx.Commit()
}
func (s *Store) SetCollaborationApplication(u User, run, id, from, to string) error {
	if to != "switching" && to != "completed" && to != "failed" && to != "uncertain" {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = collaborationRunTx(tx, u, run); err != nil {
		return err
	}
	res, err := tx.Exec("UPDATE collaboration_applications SET state=?,updated_at=? WHERE run_id=? AND action_id=? AND state=?", to, usageTimestamp(time.Now()), run, id, from)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrChatConflict
	}
	if err = collaborationEventTx(tx, run, "application", map[string]string{"state": to, "action_id": id}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CollaborationApplicationByID(u User, run, id string) (CollaborationApplication, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return CollaborationApplication{}, err
	}
	var a CollaborationApplication
	err := s.db.QueryRow("SELECT run_id,action_id,session_id,snapshot_id,state FROM collaboration_applications WHERE run_id=? AND action_id=?", run, id).Scan(&a.RunID, &a.ActionID, &a.SessionID, &a.SnapshotID, &a.State)
	return a, err
}

// Explicit abandonment fences a late application that has not reached its
// durable claim. A new action ID can be used after resolving the source issue.
func (s *Store) AbandonCollaborationApplication(u User, run, id, snapshot string) error {
	if !collaboration.ValidID(id) || !collaboration.ValidID(snapshot) {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	var n int
	if err = tx.QueryRow("SELECT COUNT(*) FROM collaboration_applications WHERE run_id=? AND state='completed'", run).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return ErrChatConflict
	}
	if err = tx.QueryRow("SELECT COUNT(*) FROM collaboration_applications WHERE run_id=? AND action_id=?", run, id).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return ErrChatConflict
	}
	now := usageTimestamp(time.Now())
	if _, err = tx.Exec("INSERT INTO collaboration_applications(run_id,action_id,session_id,snapshot_id,state,created_at,updated_at) VALUES(?,?,?,?,'failed',?,?)", run, id, r.SourceSessionID, snapshot, now, now); err != nil {
		return err
	}
	return tx.Commit()
}
