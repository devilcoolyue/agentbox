package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentbox/internal/collaboration"
)

type CollaborationEvent struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Body      json.RawMessage `json:"body"`
	CreatedAt string          `json:"created_at"`
}

func (s *Store) CollaborationEvents(u User, run string, after int64) ([]CollaborationEvent, error) {
	if after < 0 {
		return nil, ErrChatInvalid
	}
	if _, err := s.Collaboration(u, run); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT seq,type,body_json,created_at FROM collaboration_events WHERE run_id=? AND seq>? ORDER BY seq LIMIT 100", run, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaborationEvent{}
	for rows.Next() {
		var event CollaborationEvent
		var raw string
		if err = rows.Scan(&event.Seq, &event.Type, &raw, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Body = json.RawMessage(raw)
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) AbandonCollaborationRequest(u User, id string) error {
	if !collaboration.ValidID(id) {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return err
	}
	var state string
	err = tx.QueryRow("SELECT state FROM collaborations WHERE user=? AND user_created_at=? AND request_id=?", u.Name, ownerEpoch(u), id).Scan(&state)
	if err == nil {
		if state == CollabDeleted {
			return nil
		}
		return ErrChatConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := usageTimestamp(time.Now())
	_, err = tx.Exec(`INSERT INTO collaborations(id,user,user_created_at,source_session_id,request_id,fingerprint,request_json,state,created_at,updated_at) VALUES(?,?,?,'',?,'','null','deleted',?,?)`, NewCollaborationID(), u.Name, ownerEpoch(u), id, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}
