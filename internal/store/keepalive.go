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
}

// Keepalive returns the workspace's keepalive setting; off when it never set one.
func (s *Store) Keepalive(sessionID string) (WorkspaceKeepalive, error) {
	var k WorkspaceKeepalive
	err := s.db.QueryRow("SELECT enabled,terminal,max_hours,min_context_tokens FROM workspace_keepalive WHERE session_id=?", sessionID).
		Scan(&k.Enabled, &k.Terminal, &k.MaxHours, &k.MinContextTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceKeepalive{}, nil
	}
	return k, err
}

// SetKeepalive saves the workspace's keepalive setting. A workspace deleted
// meanwhile gets no row.
func (s *Store) SetKeepalive(sessionID string, k WorkspaceKeepalive) error {
	res, err := s.db.Exec(`INSERT INTO workspace_keepalive(session_id,enabled,terminal,max_hours,min_context_tokens,updated_at)
		SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM sessions WHERE id=?)
		ON CONFLICT(session_id) DO UPDATE SET enabled=excluded.enabled,terminal=excluded.terminal,
		max_hours=excluded.max_hours,min_context_tokens=excluded.min_context_tokens,updated_at=excluded.updated_at`,
		sessionID, k.Enabled, k.Terminal, k.MaxHours, k.MinContextTokens, usageTimestamp(time.Now()), sessionID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrKeepaliveWorkspace
	}
	return nil
}
