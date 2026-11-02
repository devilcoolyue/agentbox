package store

import (
	"time"
)

// Login methods: how a login token came to be issued.
const (
	LoginPassword = "password"
	LoginDesktop  = "desktop"
	LoginLink     = "abox-link"
)

// loginEventKeep bounds the login history. An IP address and browser per login
// is personal data, so it does not stay forever just because nothing asked for
// it to go.
const loginEventKeep = 180 * 24 * time.Hour

// LoginEvent is one issued login token. At is Unix milliseconds.
type LoginEvent struct {
	ID        int64  `json:"id"`
	User      string `json:"user"`
	At        int64  `json:"at"`
	Method    string `json:"method"`
	IP        string `json:"ip"`
	UserAgent string `json:"user_agent"`
}

// RecordLogin appends a login. Selecting through users keeps an account deleted
// while the login was finishing from leaving a row nothing would clean up.
func (s *Store) RecordLogin(e LoginEvent) error {
	_, err := s.db.Exec(`INSERT INTO login_events(user, at, method, ip, user_agent)
		SELECT name, ?, ?, ?, ? FROM users WHERE name = ?`,
		e.At, e.Method, e.IP, e.UserAgent, e.User)
	return err
}

// LoginEvents lists logins newest first. user "" means everyone; before > 0
// continues after that event ID.
func (s *Store) LoginEvents(user string, before int64, limit int) ([]LoginEvent, error) {
	query := "SELECT id, user, at, method, ip, user_agent FROM login_events WHERE 1=1"
	args := []any{}
	if user != "" {
		query += " AND user = ?"
		args = append(args, user)
	}
	if before > 0 {
		query += " AND id < ?"
		args = append(args, before)
	}
	rows, err := s.db.Query(query+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoginEvent{}
	for rows.Next() {
		var e LoginEvent
		if err := rows.Scan(&e.ID, &e.User, &e.At, &e.Method, &e.IP, &e.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeLoginEvents deletes logins past loginEventKeep.
func (s *Store) PurgeLoginEvents(now time.Time) (int64, error) {
	res, err := s.db.Exec("DELETE FROM login_events WHERE at < ?", now.Add(-loginEventKeep).UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchUserActivity moves a user's last human action forward. Several tabs report
// independently, so an older write can land after a newer one and must not move
// the time back.
func (s *Store) TouchUserActivity(user string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO user_activity(user, last_active_at)
		SELECT name, ? FROM users WHERE name = ?
		ON CONFLICT(user) DO UPDATE SET last_active_at = MAX(last_active_at, excluded.last_active_at)`,
		at.UnixMilli(), user)
	return err
}

// UserActivity is one account's row in the admin panel. The times are Unix
// milliseconds, 0 when nothing has been recorded.
type UserActivity struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	LastActiveAt int64  `json:"last_active_at"`
	LastLoginAt  int64  `json:"last_login_at"`
	LastLoginIP  string `json:"last_login_ip"`
}

// ListUserActivity returns every account, the most recently active first.
func (s *Store) ListUserActivity() ([]UserActivity, error) {
	rows, err := s.db.Query(`SELECT u.name, u.role, COALESCE(a.last_active_at, 0), COALESCE(l.at, 0), COALESCE(l.ip, '')
		FROM users u
		LEFT JOIN user_activity a ON a.user = u.name
		LEFT JOIN login_events l ON l.id = (SELECT MAX(id) FROM login_events WHERE user = u.name)
		ORDER BY COALESCE(a.last_active_at, 0) DESC, COALESCE(l.at, 0) DESC, u.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserActivity{}
	for rows.Next() {
		var a UserActivity
		if err := rows.Scan(&a.Name, &a.Role, &a.LastActiveAt, &a.LastLoginAt, &a.LastLoginIP); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
