// Package store persists session metadata in a SQLite database. On first
// open it imports any legacy state.json sitting next to the database file,
// then renames it out of the way so the import runs only once.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StatusStopped = "stopped"
	StatusRunning = "running"

	RoleAdmin = "admin"
	RoleUser  = "user"
)

type Session struct {
	ID          string `json:"id"`
	User        string `json:"user"`
	Name        string `json:"name"`
	Agent       string `json:"agent"` // "claude" | "codex"
	AccountID   string `json:"account_id"`
	ContainerID string `json:"container_id,omitempty"`
	Status      string `json:"status"`
	// ChatSession is the provider-side conversation id of the latest headless
	// turn; each new turn resumes from it so the conversation survives
	// container restarts.
	ChatSession string    `json:"chat_session,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id           TEXT PRIMARY KEY,
	user         TEXT NOT NULL,
	name         TEXT NOT NULL,
	agent        TEXT NOT NULL,
	account_id   TEXT NOT NULL,
	container_id TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL,
	chat_session TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user);
CREATE TABLE IF NOT EXISTS users (
	name       TEXT PRIMARY KEY,
	role       TEXT NOT NULL,
	pass_hash  TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	token      TEXT PRIMARY KEY,
	user       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tokens_user ON tokens(user);
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// The driver serializes access per connection; a single connection keeps
	// writes ordered and sidesteps SQLITE_BUSY between our own connections.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.importLegacyJSON(filepath.Join(filepath.Dir(path), "state.json")); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// importLegacyJSON migrates sessions from the pre-SQLite state file. Existing
// rows win over the file so a crash between import and rename cannot undo
// newer writes; the file is renamed to *.migrated afterwards.
func (s *Store) importLegacyJSON(jsonPath string) error {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var legacy struct {
		Sessions map[string]*Session `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return fmt.Errorf("parse legacy %s: %w", jsonPath, err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sess := range legacy.Sessions {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO sessions
			(id, user, name, agent, account_id, container_id, status, chat_session, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, sess.User, sess.Name, sess.Agent, sess.AccountID, sess.ContainerID,
			sess.Status, sess.ChatSession,
			sess.CreatedAt.Format(time.RFC3339Nano), sess.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("import legacy session %s: %w", sess.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return os.Rename(jsonPath, jsonPath+".migrated")
}

func NewID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

const sessionCols = "id, user, name, agent, account_id, container_id, status, chat_session, created_at, updated_at"

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var sess Session
	var created, updated string
	if err := row.Scan(&sess.ID, &sess.User, &sess.Name, &sess.Agent, &sess.AccountID,
		&sess.ContainerID, &sess.Status, &sess.ChatSession, &created, &updated); err != nil {
		return Session{}, err
	}
	var err error
	if sess.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Session{}, fmt.Errorf("session %s created_at: %w", sess.ID, err)
	}
	if sess.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Session{}, fmt.Errorf("session %s updated_at: %w", sess.ID, err)
	}
	return sess, nil
}

func (s *Store) query(where string, args ...any) []Session {
	rows, err := s.db.Query("SELECT "+sessionCols+" FROM sessions"+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	return out
}

func (s *Store) List(user string) []Session {
	return s.query(" WHERE user = ? ORDER BY created_at DESC", user)
}

func (s *Store) Get(id string) (Session, bool) {
	sess, err := scanSession(s.db.QueryRow("SELECT "+sessionCols+" FROM sessions WHERE id = ?", id))
	if err != nil {
		return Session{}, false
	}
	return sess, true
}

func (s *Store) put(exec interface {
	Exec(string, ...any) (sql.Result, error)
}, sess Session) error {
	_, err := exec.Exec(`INSERT INTO sessions
		(id, user, name, agent, account_id, container_id, status, chat_session, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			user=excluded.user, name=excluded.name, agent=excluded.agent,
			account_id=excluded.account_id, container_id=excluded.container_id,
			status=excluded.status, chat_session=excluded.chat_session,
			created_at=excluded.created_at, updated_at=excluded.updated_at`,
		sess.ID, sess.User, sess.Name, sess.Agent, sess.AccountID, sess.ContainerID,
		sess.Status, sess.ChatSession,
		sess.CreatedAt.Format(time.RFC3339Nano), sess.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) Put(sess Session) error {
	sess.UpdatedAt = time.Now()
	return s.put(s.db, sess)
}

// Update applies fn to the session inside a transaction and persists the result.
func (s *Store) Update(id string, fn func(*Session)) (Session, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRow("SELECT "+sessionCols+" FROM sessions WHERE id = ?", id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, fmt.Errorf("session %s not found", id)
		}
		return Session{}, err
	}
	fn(&sess)
	sess.UpdatedAt = time.Now()
	if err := s.put(tx, sess); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	return sess, nil
}

func (s *Store) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

// All returns every session regardless of user (used for startup reconcile).
func (s *Store) All() []Session {
	return s.query("")
}

// SessionCounts returns the number of sessions per user.
func (s *Store) SessionCounts() map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query("SELECT user, COUNT(*) FROM sessions GROUP BY user")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var user string
		var n int
		if rows.Scan(&user, &n) == nil {
			out[user] = n
		}
	}
	return out
}

// ReassignUser moves every session of one user to another (one-time seed
// migration from the single-user era).
func (s *Store) ReassignUser(from, to string) error {
	_, err := s.db.Exec("UPDATE sessions SET user = ? WHERE user = ?", to, from)
	return err
}

// --- users & login tokens ---

type User struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"` // RoleAdmin | RoleUser
	PassHash  string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) CountUsers() int {
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	return n
}

func (s *Store) CreateUser(u User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	_, err := s.db.Exec("INSERT INTO users (name, role, pass_hash, created_at) VALUES (?, ?, ?, ?)",
		u.Name, u.Role, u.PassHash, u.CreatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetUser(name string) (User, bool) {
	var u User
	var created string
	err := s.db.QueryRow("SELECT name, role, pass_hash, created_at FROM users WHERE name = ?", name).
		Scan(&u.Name, &u.Role, &u.PassHash, &created)
	if err != nil {
		return User{}, false
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return u, true
}

func (s *Store) ListUsers() []User {
	rows, err := s.db.Query("SELECT name, role, pass_hash, created_at FROM users ORDER BY created_at")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created string
		if rows.Scan(&u.Name, &u.Role, &u.PassHash, &created) != nil {
			continue
		}
		u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, u)
	}
	return out
}

// DeleteUser removes the user together with all their login tokens.
func (s *Store) DeleteUser(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM tokens WHERE user = ?", name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM users WHERE name = ?", name); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetPassword(name, hash string) error {
	res, err := s.db.Exec("UPDATE users SET pass_hash = ? WHERE name = ?", hash, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user %s not found", name)
	}
	return nil
}

func (s *Store) CreateToken(token, user string) error {
	_, err := s.db.Exec("INSERT INTO tokens (token, user, created_at) VALUES (?, ?, ?)",
		token, user, time.Now().Format(time.RFC3339Nano))
	return err
}

// TokenUser resolves a login token to its user; the join makes tokens of a
// deleted user dead even if a stray row survived.
func (s *Store) TokenUser(token string) (User, bool) {
	var u User
	err := s.db.QueryRow(`SELECT u.name, u.role, u.pass_hash FROM tokens t
		JOIN users u ON u.name = t.user WHERE t.token = ?`, token).
		Scan(&u.Name, &u.Role, &u.PassHash)
	if err != nil {
		return User{}, false
	}
	return u, true
}

func (s *Store) DeleteToken(token string) error {
	_, err := s.db.Exec("DELETE FROM tokens WHERE token = ?", token)
	return err
}

// DeleteUserTokensExcept revokes every login token of a user except keep
// (pass "" to revoke all). Used on password change so other browsers drop off.
func (s *Store) DeleteUserTokensExcept(user, keep string) error {
	_, err := s.db.Exec("DELETE FROM tokens WHERE user = ? AND token != ?", user, keep)
	return err
}
