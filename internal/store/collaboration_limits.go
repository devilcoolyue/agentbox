package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

type CollaborationLimits struct {
	StepTimeoutSeconds  int `json:"step_timeout_seconds,omitempty"`
	TotalRuntimeSeconds int `json:"total_runtime_seconds,omitempty"`
	WaitTimeoutSeconds  int `json:"wait_timeout_seconds,omitempty"`
}

func (l CollaborationLimits) Valid() bool {
	return (l.StepTimeoutSeconds == 0 || l.StepTimeoutSeconds >= 10 && l.StepTimeoutSeconds <= 1800) && (l.TotalRuntimeSeconds == 0 || l.TotalRuntimeSeconds >= 10 && l.TotalRuntimeSeconds <= 86400) && (l.WaitTimeoutSeconds == 0 || l.WaitTimeoutSeconds >= 60 && l.WaitTimeoutSeconds <= 30*86400)
}
func (l CollaborationLimits) StepTimeout() time.Duration {
	if l.StepTimeoutSeconds == 0 {
		return 30 * time.Minute
	}
	return time.Duration(l.StepTimeoutSeconds) * time.Second
}
func (l CollaborationLimits) TotalRuntime() time.Duration {
	if l.TotalRuntimeSeconds == 0 {
		return 6 * time.Hour
	}
	return time.Duration(l.TotalRuntimeSeconds) * time.Second
}
func (l CollaborationLimits) WaitTimeout() time.Duration {
	if l.WaitTimeoutSeconds == 0 {
		return 7 * 24 * time.Hour
	}
	return time.Duration(l.WaitTimeoutSeconds) * time.Second
}

func collaborationRuntimeTx(tx *sql.Tx, run string) (time.Duration, error) {
	var millis int64
	err := tx.QueryRow(`SELECT COALESCE(SUM(MAX(0,CAST((julianday(ended_at)-julianday(started_at))*86400000 AS INTEGER))),0) FROM collaboration_steps WHERE run_id=? AND started_at!='' AND state NOT IN ('accepted','starting','running','finalizing')`, run).Scan(&millis)
	return time.Duration(millis) * time.Millisecond, err
}
func (s *Store) CollaborationRuntimeRemaining(u User, run string) (time.Duration, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return 0, err
	}
	var raw string
	var cfg CollaborationConfig
	if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", run, r.ConfigVersion).Scan(&raw); err != nil {
		return 0, err
	}
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return 0, err
	}
	used, err := collaborationRuntimeTx(tx, run)
	return max(0, cfg.TotalRuntime()-used), err
}
func pauseForCollaborationTimeTx(tx *sql.Tx, r *CollaborationRun) (bool, error) {
	var raw string
	var cfg CollaborationConfig
	if err := tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", r.ID, r.ConfigVersion).Scan(&raw); err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return false, err
	}
	used, err := collaborationRuntimeTx(tx, r.ID)
	if err != nil {
		return false, err
	}
	if used >= cfg.TotalRuntime() {
		pauseCollaboration(r, "runtime_limit")
		return true, nil
	}
	return false, nil
}

// Waiting consumes no container. Expiration pauses rather than deleting data
// or silently answering a question. A user explicitly resumes the wait.
func (s *Store) ExpireCollaborationWaits(now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT " + collaborationCols + " FROM collaborations WHERE state IN ('waiting_user','awaiting_plan')")
	if err != nil {
		return err
	}
	var runs []CollaborationRun
	for rows.Next() {
		r, e := scanCollaboration(rows)
		if e != nil {
			rows.Close()
			return e
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range runs {
		var raw string
		var cfg CollaborationConfig
		if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", r.ID, r.ConfigVersion).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
			return err
		}
		since, e := time.Parse(time.RFC3339Nano, r.UpdatedAt)
		if e != nil {
			return e
		}
		if now.Sub(since) >= cfg.WaitTimeout() {
			pauseCollaboration(&r, "wait_expired")
			if err = saveCollaborationTx(tx, &r); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
