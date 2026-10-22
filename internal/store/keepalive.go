package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrKeepaliveWorkspace reports a keepalive setting saved for a workspace
// that no longer exists.
var ErrKeepaliveWorkspace = errors.New("workspace not found")

// WorkspaceKeepalive is a workspace's own prompt cache keepalive setting (see
// server/keepalive.go). The zero value is off; zero limits follow the
// administrator's defaults.
type WorkspaceKeepalive struct {
	Enabled bool
	// Terminal also keeps the caches of the Claude Code sessions run in the
	// workspace's terminal.
	Terminal         bool
	MaxHours         int
	MinContextTokens int64
	// PausePercent is the five-hour usage at which the workspace's chat
	// pauses: 0 follows the administrator's default, -1 is off.
	PausePercent int
}

// Keepalive returns the workspace's keepalive setting; off when it never set one.
func (s *Store) Keepalive(sessionID string) (WorkspaceKeepalive, error) {
	var k WorkspaceKeepalive
	err := s.db.QueryRow("SELECT enabled,terminal,max_hours,min_context_tokens,pause_percent FROM workspace_keepalive WHERE session_id=?", sessionID).
		Scan(&k.Enabled, &k.Terminal, &k.MaxHours, &k.MinContextTokens, &k.PausePercent)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceKeepalive{}, nil
	}
	return k, err
}

// SetKeepalive saves the workspace's keepalive setting. A workspace deleted
// meanwhile gets no row.
func (s *Store) SetKeepalive(sessionID string, k WorkspaceKeepalive) error {
	res, err := s.db.Exec(`INSERT INTO workspace_keepalive(session_id,enabled,terminal,max_hours,min_context_tokens,pause_percent,updated_at)
		SELECT ?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM sessions WHERE id=?)
		ON CONFLICT(session_id) DO UPDATE SET enabled=excluded.enabled,terminal=excluded.terminal,
		max_hours=excluded.max_hours,min_context_tokens=excluded.min_context_tokens,pause_percent=excluded.pause_percent,updated_at=excluded.updated_at`,
		sessionID, k.Enabled, k.Terminal, k.MaxHours, k.MinContextTokens, k.PausePercent, usageTimestamp(time.Now()), sessionID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrKeepaliveWorkspace
	}
	return nil
}

// RateWindow is the latest five-hour window an account's responses reported.
type RateWindow struct {
	Percent  float64
	ResetsAt time.Time
}

// SetRateWindow saves an account's latest five-hour window.
func (s *Store) SetRateWindow(account string, w RateWindow) error {
	_, err := s.db.Exec(`INSERT INTO account_rate_windows(account_id,percent,resets_at,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET percent=excluded.percent,resets_at=excluded.resets_at,updated_at=excluded.updated_at`,
		account, w.Percent, usageTimestamp(w.ResetsAt), usageTimestamp(time.Now()))
	return err
}

// RateWindows returns every account's window that has not started over yet.
func (s *Store) RateWindows(now time.Time) (map[string]RateWindow, error) {
	rows, err := s.db.Query("SELECT account_id,percent,resets_at FROM account_rate_windows WHERE resets_at>?", usageTimestamp(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RateWindow{}
	for rows.Next() {
		var id, at string
		var w RateWindow
		if err := rows.Scan(&id, &w.Percent, &at); err != nil {
			return nil, err
		}
		if w.ResetsAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			continue
		}
		out[id] = w
	}
	return out, rows.Err()
}
