package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type GitOperation struct {
	ID           int64  `json:"id"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
	Actor        string `json:"actor"`
	SessionID    string `json:"session_id"`
	Repo         string `json:"repo"`
	ConnectionID string `json:"connection_id"`
	Operation    string `json:"operation"`
	Target       string `json:"target"`
	Result       string `json:"result"`
}

// GitOperations uses the authenticated actor as a hard scope and an ID cursor;
// callers cannot enumerate other users' repositories by passing query filters.
func (s *Store) GitOperations(actor string, before int64, limit int) ([]GitOperation, error) {
	if limit < 1 || limit > 100 || before < 0 {
		return nil, fmt.Errorf("invalid Git operation page")
	}
	rows, err := s.db.Query(`SELECT id,ts,finished_at,actor,session_id,repo,connection_id,operation,target,result FROM git_operations WHERE actor=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, actor, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitOperation{}
	for rows.Next() {
		var op GitOperation
		if err = rows.Scan(&op.ID, &op.StartedAt, &op.FinishedAt, &op.Actor, &op.SessionID, &op.Repo, &op.ConnectionID, &op.Operation, &op.Target, &op.Result); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// Called once during server startup under data_dir's exclusive lock. A previous
// interrupted push may have reached upstream, so never label it as rolled back.
func (s *Store) RecoverGitOperations() error {
	_, err := s.db.Exec("UPDATE git_operations SET result='interrupted_unknown',finished_at=? WHERE result='running'", time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

type GitFetchState struct {
	Target string `json:"target"`
	At     string `json:"at"`
}

func (s *Store) LastGitFetch(session, repo string) (*GitFetchState, error) {
	var state GitFetchState
	err := s.db.QueryRow(`SELECT target,finished_at FROM git_operations WHERE session_id=? AND repo=? AND operation IN ('fetch','pull') AND result='success' AND finished_at!='' ORDER BY id DESC LIMIT 1`, session, repo).Scan(&state.Target, &state.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}
