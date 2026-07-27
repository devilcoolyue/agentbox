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
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// UsageKindChat is spend from a turn the user asked for; UsageKindTitle is
	// spend from the thread-title summary the server runs on its own.
	UsageKindChat  = "chat"
	UsageKindTitle = "title"

	StatusStopped = "stopped"
	StatusRunning = "running"

	// StopIdle marks a container the idle reaper stopped (as opposed to a stop
	// the user asked for), so the UI can present it as sleeping and wake it.
	StopIdle = "idle"

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
	ChatSession string `json:"chat_session,omitempty"`
	// StopReason explains why a stopped session is stopped: "idle" when the
	// reaper put it to sleep, "" when the user stopped it (or it never ran).
	// Lets the UI show "已休眠" instead of a stop the user didn't ask for.
	StopReason string    `json:"stop_reason,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
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
CREATE TABLE IF NOT EXISTS usage_events (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	ts                 TEXT    NOT NULL,
	user               TEXT    NOT NULL,
	session_id         TEXT    NOT NULL,
	thread_id          TEXT    NOT NULL DEFAULT '',
	turn_id            TEXT    NOT NULL DEFAULT '',
	agent              TEXT    NOT NULL DEFAULT '',
	account_id         TEXT    NOT NULL DEFAULT '',
	model              TEXT    NOT NULL DEFAULT '',
	kind               TEXT    NOT NULL DEFAULT 'chat',
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micro_usd     INTEGER NOT NULL DEFAULT 0,
	duration_ms        INTEGER NOT NULL DEFAULT 0,
	raw                TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_user_ts ON usage_events(user, ts);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage_events(session_id);
CREATE INDEX IF NOT EXISTS idx_usage_turn ON usage_events(turn_id);
CREATE TABLE IF NOT EXISTS quotas (
	user              TEXT PRIMARY KEY,
	enforced          INTEGER NOT NULL DEFAULT 1,
	balance_micro_usd INTEGER NOT NULL DEFAULT 0,
	granted_micro_usd INTEGER NOT NULL DEFAULT 0,
	spent_micro_usd   INTEGER NOT NULL DEFAULT 0,
	created_at        TEXT NOT NULL,
	updated_at        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS credit_ledger (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	ts              TEXT    NOT NULL,
	user            TEXT    NOT NULL,
	ref             TEXT    NOT NULL,
	reason          TEXT    NOT NULL,
	delta_micro_usd INTEGER NOT NULL,
	balance_after   INTEGER NOT NULL,
	note            TEXT    NOT NULL DEFAULT '',
	actor           TEXT    NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_ref ON credit_ledger(ref);
CREATE INDEX IF NOT EXISTS idx_ledger_user_ts ON credit_ledger(user, ts);
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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.importLegacyJSON(filepath.Join(filepath.Dir(path), "state.json")); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate applies additive column changes the CREATE TABLE above can't make to
// an existing database. Each entry is idempotent: re-adding a column errors
// with "duplicate column name", which is the already-applied case.
func migrate(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE sessions ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_events ADD COLUMN kind TEXT NOT NULL DEFAULT 'chat'`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %q: %w", stmt, err)
		}
	}
	return nil
}

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
			(id, user, name, agent, account_id, container_id, status, chat_session, stop_reason, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, sess.User, sess.Name, sess.Agent, sess.AccountID, sess.ContainerID,
			sess.Status, sess.ChatSession, sess.StopReason,
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

const sessionCols = "id, user, name, agent, account_id, container_id, status, chat_session, stop_reason, created_at, updated_at"

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var sess Session
	var created, updated string
	if err := row.Scan(&sess.ID, &sess.User, &sess.Name, &sess.Agent, &sess.AccountID,
		&sess.ContainerID, &sess.Status, &sess.ChatSession, &sess.StopReason, &created, &updated); err != nil {
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
		(id, user, name, agent, account_id, container_id, status, chat_session, stop_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			user=excluded.user, name=excluded.name, agent=excluded.agent,
			account_id=excluded.account_id, container_id=excluded.container_id,
			status=excluded.status, chat_session=excluded.chat_session,
			stop_reason=excluded.stop_reason,
			created_at=excluded.created_at, updated_at=excluded.updated_at`,
		sess.ID, sess.User, sess.Name, sess.Agent, sess.AccountID, sess.ContainerID,
		sess.Status, sess.ChatSession, sess.StopReason,
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

// DeleteUser removes the user together with all their login tokens and their
// credit balance. The ledger and usage rows stay: they are the record of money
// that was actually spent. Dropping the quota row matters — a later user of the
// same name must not inherit the old balance.
func (s *Store) DeleteUser(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM tokens WHERE user = ?", name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM quotas WHERE user = ?", name); err != nil {
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

// TokenTTL bounds how long a login token stays valid after issue. Absolute
// (not sliding) so a leaked token can't be kept alive forever by being used.
const TokenTTL = 30 * 24 * time.Hour

func (s *Store) CreateToken(token, user string) error {
	_, err := s.db.Exec("INSERT INTO tokens (token, user, created_at) VALUES (?, ?, ?)",
		token, user, time.Now().Format(time.RFC3339Nano))
	return err
}

// TokenUser resolves a login token to its user; the join makes tokens of a
// deleted user dead even if a stray row survived. Tokens older than TokenTTL
// are treated as invalid and deleted lazily on access.
func (s *Store) TokenUser(token string) (User, bool) {
	if token == "" {
		return User{}, false
	}
	var u User
	var created string
	err := s.db.QueryRow(`SELECT u.name, u.role, u.pass_hash, t.created_at FROM tokens t
		JOIN users u ON u.name = t.user WHERE t.token = ?`, token).
		Scan(&u.Name, &u.Role, &u.PassHash, &created)
	if err != nil {
		return User{}, false
	}
	if ts, perr := time.Parse(time.RFC3339Nano, created); perr == nil && time.Since(ts) > TokenTTL {
		_, _ = s.db.Exec("DELETE FROM tokens WHERE token = ?", token)
		return User{}, false
	}
	return u, true
}

// PurgeExpiredTokens deletes every login token past TokenTTL. Called
// periodically so expired rows don't accumulate.
func (s *Store) PurgeExpiredTokens() (int64, error) {
	cutoff := time.Now().Add(-TokenTTL).Format(time.RFC3339Nano)
	res, err := s.db.Exec("DELETE FROM tokens WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
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

// --- usage metering ---

// UsageEvent is one provider-reported usage record for a single model inside a
// single chat turn. A turn writes several rows when the agent ran more than one
// model (Claude reports sub-agent models separately); rows of one turn share
// TurnID.
//
// The token columns are disjoint buckets, in Claude's sense of the words:
// InputTokens counts only input that missed the cache, CacheReadTokens the part
// served from cache, CacheWriteTokens the part written into it. Codex instead
// reports its cached count as a subset of the input count, so the writer
// subtracts it out before storing (see server.parseUsage). Raw keeps the
// provider's untouched event so a normalization that turns out wrong can be
// re-derived from history rather than lost.
type UsageEvent struct {
	ID        int64     `json:"id"`
	TS        time.Time `json:"ts"`
	User      string    `json:"user"`
	SessionID string    `json:"session_id"`
	ThreadID  string    `json:"thread_id,omitempty"`
	TurnID    string    `json:"turn_id"`
	Agent     string    `json:"agent"`
	AccountID string    `json:"account_id,omitempty"`
	// Model is the provider's model id. Empty means the turn ran on the
	// provider-side default and the event did not name it (Codex).
	Model string `json:"model,omitempty"`
	// Kind separates what the spend was for: UsageKindChat is the user's own
	// turn, UsageKindTitle the thread-title summary the server triggers on its
	// own. Both can land on the same cheap model, so the model id alone cannot
	// tell them apart in a report.
	Kind             string `json:"kind"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	// CostMicroUSD is the provider-reported cost in millionths of a USD. Money
	// is kept off float64 so summing a report stays exact. Codex reports no
	// cost at all: those rows carry 0 and have to be priced from tokens.
	CostMicroUSD int64 `json:"cost_micro_usd"`
	// DurationMS is a turn-level measure repeated on every row of the turn.
	// Aggregating it across rows means MAX per turn_id, never SUM.
	DurationMS int64 `json:"duration_ms"`
	// Raw is the provider event verbatim, stored on the turn's first row only.
	Raw string `json:"-"`
}

// InsertUsage appends usage rows and charges them against the users' credit in
// one transaction, so a multi-model turn lands whole or not at all — and so a
// recorded row can never end up unbilled.
//
// Users without a quota row are unmetered: their usage is still recorded, no
// credit moves. Rows costing 0 (Codex with no price configured) record but
// charge nothing. The idempotency key of each charge is the usage row's own id,
// which makes a later re-pricing pass safe to write: the same ref can't debit
// twice (see applyCreditTx).
//
// That key is only sound because usage_events.id is AUTOINCREMENT: a plain
// rowid gets reused once the newest rows are deleted, and a reused id would
// collide with the old usage:<id> ledger ref — the charge would be skipped as a
// replay and that usage would go free. Don't drop the keyword.
func (s *Store) InsertUsage(evs ...UsageEvent) error {
	if len(evs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	metered := map[string]bool{}
	for _, e := range evs {
		if e.TS.IsZero() {
			e.TS = time.Now()
		}
		if e.Kind == "" {
			e.Kind = UsageKindChat
		}
		res, err := tx.Exec(`INSERT INTO usage_events
			(ts, user, session_id, thread_id, turn_id, agent, account_id, model, kind,
			 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			 cost_micro_usd, duration_ms, raw)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.TS.Format(time.RFC3339Nano), e.User, e.SessionID, e.ThreadID, e.TurnID,
			e.Agent, e.AccountID, e.Model, e.Kind,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens,
			e.CostMicroUSD, e.DurationMS, e.Raw)
		if err != nil {
			return fmt.Errorf("insert usage %s/%s: %w", e.SessionID, e.TurnID, err)
		}
		if e.CostMicroUSD <= 0 {
			continue
		}
		ok, seen := metered[e.User]
		if !seen {
			ok = quotaExistsTx(tx, e.User)
			metered[e.User] = ok
		}
		if !ok {
			continue
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("usage id %s/%s: %w", e.SessionID, e.TurnID, err)
		}
		note := e.Model
		if note == "" {
			note = e.Agent
		}
		if _, err := applyCreditTx(tx, e.User, -e.CostMicroUSD,
			fmt.Sprintf("usage:%d", id), ReasonSpend, note, ""); err != nil {
			return fmt.Errorf("charge usage %d: %w", id, err)
		}
	}
	return tx.Commit()
}

const usageCols = `id, ts, user, session_id, thread_id, turn_id, agent, account_id, model, kind,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	cost_micro_usd, duration_ms, raw`

// UsageFilter narrows a usage query. The zero value matches everything.
type UsageFilter struct {
	User      string    // empty: every user
	SessionID string    // empty: every session
	Kind      string    // empty: every kind
	Since     time.Time // zero: no lower bound (inclusive)
	Until     time.Time // zero: no upper bound (exclusive)
	Limit     int       // <= 0: no limit
}

// ListUsage returns matching usage rows, newest first.
func (s *Store) ListUsage(f UsageFilter) []UsageEvent {
	q := "SELECT " + usageCols + " FROM usage_events WHERE 1=1"
	var args []any
	if f.User != "" {
		q += " AND user = ?"
		args = append(args, f.User)
	}
	if f.SessionID != "" {
		q += " AND session_id = ?"
		args = append(args, f.SessionID)
	}
	if f.Kind != "" {
		q += " AND kind = ?"
		args = append(args, f.Kind)
	}
	if !f.Since.IsZero() {
		q += " AND ts >= ?"
		args = append(args, f.Since.Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		q += " AND ts < ?"
		args = append(args, f.Until.Format(time.RFC3339Nano))
	}
	q += " ORDER BY ts DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []UsageEvent
	for rows.Next() {
		var e UsageEvent
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.User, &e.SessionID, &e.ThreadID, &e.TurnID,
			&e.Agent, &e.AccountID, &e.Model, &e.Kind,
			&e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheWriteTokens,
			&e.CostMicroUSD, &e.DurationMS, &e.Raw); err != nil {
			continue
		}
		e.TS, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, e)
	}
	return out
}
