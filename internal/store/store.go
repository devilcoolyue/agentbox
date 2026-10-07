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
	// UsageKindChat is spend from a turn the user asked for in the web chat;
	// UsageKindTitle is spend from the thread-title summary the server runs on
	// its own. UsageKindTerminal is spend from the CLI the user drove by hand in
	// the terminal tab — it never passes through the server, so those rows are
	// backfilled from the CLI's own transcript files and, unlike the other two,
	// never move the user's balance (see UpsertTerminalUsage).
	UsageKindChat     = "chat"
	UsageKindTitle    = "title"
	UsageKindTerminal = "terminal"

	StatusStopped = "stopped"
	StatusRunning = "running"

	// StopIdle marks a container the idle reaper stopped (as opposed to a stop
	// the user asked for), so the UI can present it as sleeping and wake it.
	StopIdle = "idle"

	RoleAdmin = "admin"
	RoleUser  = "user"
)

type Session struct {
	ID        string `json:"id"`
	User      string `json:"user"`
	Name      string `json:"name"`
	Agent     string `json:"agent"` // "claude" | "codex"
	AccountID string `json:"account_id"`
	// DefaultModel snapshots the system default when this workspace is created.
	DefaultModel string `json:"default_model"`
	ContainerID  string `json:"container_id,omitempty"`
	Status       string `json:"status"`
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

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// FULL syncs the WAL on every commit: an acknowledged receipt must survive
	// before an external runner can start. Put this connection-local setting in
	// the DSN too so a replacement connection cannot silently revert to NORMAL.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// The driver serializes access per connection; a single connection keeps
	// writes ordered and sidesteps SQLITE_BUSY between our own connections.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
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
			(id, user, name, agent, account_id, container_id, status, chat_session, stop_reason, default_model, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, sess.User, sess.Name, sess.Agent, sess.AccountID, sess.ContainerID,
			sess.Status, sess.ChatSession, sess.StopReason, sess.DefaultModel,
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

const sessionCols = "id, user, name, agent, account_id, container_id, status, chat_session, stop_reason, default_model, created_at, updated_at"

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var sess Session
	var created, updated string
	if err := row.Scan(&sess.ID, &sess.User, &sess.Name, &sess.Agent, &sess.AccountID,
		&sess.ContainerID, &sess.Status, &sess.ChatSession, &sess.StopReason, &sess.DefaultModel, &created, &updated); err != nil {
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
		(id, user, name, agent, account_id, container_id, status, chat_session, stop_reason, default_model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			user=excluded.user, name=excluded.name, agent=excluded.agent,
			account_id=excluded.account_id, container_id=excluded.container_id,
			status=excluded.status, chat_session=excluded.chat_session,
			stop_reason=excluded.stop_reason, default_model=excluded.default_model,
			created_at=excluded.created_at, updated_at=excluded.updated_at`,
		sess.ID, sess.User, sess.Name, sess.Agent, sess.AccountID, sess.ContainerID,
		sess.Status, sess.ChatSession, sess.StopReason, sess.DefaultModel,
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE chat_requests SET state='deleted',request_json='null',revision=revision+1,updated_at=? WHERE session_id=? AND state!='deleted'", usageTimestamp(time.Now()), id); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE workspace_creations SET state='abandoned' WHERE session_id=?", id); err != nil {
		return err
	}
	for _, query := range []string{"DELETE FROM client_terminals WHERE session_id=?", "DELETE FROM client_projects WHERE session_id=?", "DELETE FROM git_bindings WHERE session_id=?", "DELETE FROM git_defaults WHERE session_id=?", "DELETE FROM sessions WHERE id=?"} {
		if _, err := tx.Exec(query, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if _, err := tx.Exec("UPDATE chat_requests SET state='deleted',request_json='null',revision=revision+1,updated_at=? WHERE user=? AND state!='deleted'", usageTimestamp(time.Now()), name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM tokens WHERE user = ?", name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM quotas WHERE user = ?", name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM git_profiles WHERE user = ?", name); err != nil {
		return err
	}
	for _, query := range []string{
		"DELETE FROM git_bindings WHERE connection_id IN (SELECT id FROM git_connections WHERE owner=?)",
		"DELETE FROM git_defaults WHERE user=?",
		"DELETE FROM git_defaults WHERE connection_id IN (SELECT id FROM git_connections WHERE owner=?)",
		"DELETE FROM git_connection_shares WHERE user=?",
		"DELETE FROM git_connection_shares WHERE connection_id IN (SELECT id FROM git_connections WHERE owner=?)",
		"DELETE FROM git_connections WHERE owner=?",
	} {
		if _, err := tx.Exec(query, name); err != nil {
			return err
		}
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

// ResetPasswordIfUserUnchanged updates the authenticated account and revokes
// its other tokens together. No reader can use a soon-to-be-revoked token to
// obtain the new credential identity while the reset is only half complete.
func (s *Store) ResetPasswordIfUserUnchanged(expected User, hash, keep string) (bool, error) {
	if expected.Name == "" || expected.CreatedAt.IsZero() || hash == "" {
		return false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE users SET pass_hash = ? WHERE name = ? AND pass_hash = ? AND created_at = ?`,
		hash, expected.Name, expected.PassHash, expected.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if _, err = tx.Exec("DELETE FROM tokens WHERE user = ? AND token != ?", expected.Name, keep); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// TokenTTL bounds how long a login token stays valid after issue. Absolute
// (not sliding) so a leaked token can't be kept alive forever by being used.
const TokenTTL = 30 * 24 * time.Hour

func (s *Store) CreateToken(token, user string) error {
	_, err := s.db.Exec("INSERT INTO tokens (token, user, created_at) VALUES (?, ?, ?)",
		token, user, time.Now().Format(time.RFC3339Nano))
	return err
}

// CreateTokenIfUserUnchanged issues a login only while the account still has
// the credential and creation identity that was originally authenticated. The
// predicate and insert are one SQLite statement, so password reset/deletion
// cannot slip between an application-side check and token creation. Existing
// tokens and database schema are unchanged.
func (s *Store) CreateTokenIfUserUnchanged(token string, expected User) (bool, error) {
	if token == "" || expected.Name == "" || expected.CreatedAt.IsZero() {
		return false, nil
	}
	result, err := s.db.Exec(`INSERT INTO tokens (token, user, created_at)
		SELECT ?, name, ? FROM users WHERE name = ? AND pass_hash = ? AND created_at = ?`,
		token, time.Now().Format(time.RFC3339Nano), expected.Name, expected.PassHash, expected.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// TokenUser resolves a login token to its user; the join makes tokens of a
// deleted user dead even if a stray row survived. Tokens older than TokenTTL
// are treated as invalid and deleted lazily on access.
func (s *Store) TokenUser(token string) (User, bool) {
	if token == "" {
		return User{}, false
	}
	var u User
	var created, userCreated string
	err := s.db.QueryRow(`SELECT u.name, u.role, u.pass_hash, t.created_at, u.created_at FROM tokens t
		JOIN users u ON u.name = t.user WHERE t.token = ?`, token).
		Scan(&u.Name, &u.Role, &u.PassHash, &created, &userCreated)
	if err != nil {
		return User{}, false
	}
	if ts, perr := time.Parse(time.RFC3339Nano, created); perr == nil && time.Since(ts) > TokenTTL {
		_, _ = s.db.Exec("DELETE FROM tokens WHERE token = ?", token)
		return User{}, false
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, userCreated)
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
// subtracts it out before storing (see usage.ParseUsage). Raw keeps the
// provider's untouched event so a normalization that turns out wrong can be
// re-derived from history rather than lost.
type UsageEvent struct {
	Price     *PriceSnapshot `json:"price_snapshot,omitempty"`
	ID        int64          `json:"id"`
	TS        time.Time      `json:"ts"`
	User      string         `json:"user"`
	SessionID string         `json:"session_id"`
	ThreadID  string         `json:"thread_id,omitempty"`
	TurnID    string         `json:"turn_id"`
	Agent     string         `json:"agent"`
	AccountID string         `json:"account_id,omitempty"`
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
	// DurationMS is what the provider says the turn took — model-side time
	// only, excluding the CLI's own startup. Turn-level: repeated on every row
	// of the turn, so aggregating it means MAX per turn_id, never SUM.
	DurationMS int64 `json:"duration_ms"`
	// WallMS is the same turn measured on our clock: from the container being
	// ready to the process exiting, so it includes the CLI startup (~2s for
	// Claude Code) that DurationMS leaves out. TTFTMs is measured on this same
	// clock, which is why the two are comparable and WallMS >= TTFTMs while
	// DurationMS may be smaller than either. Turn-level, same caveat as above.
	WallMS int64 `json:"wall_ms"`
	// TTFTMs is time-to-first-token, measured by us rather than reported by the
	// provider: from the container being ready to the turn's first model output
	// event. Turn-level like WallMS — same aggregation caveat. 0 means not
	// measured (title turns, or a turn that produced no output event).
	TTFTMs int64 `json:"ttft_ms"`
	// Provider is what the provider says served the turn ("firstParty" from
	// Claude's modelUsage). Empty when the event does not say — Codex never
	// does, so an empty value is not evidence of anything.
	Provider string `json:"provider,omitempty"`
	// ReqID is the dedup key of a backfilled terminal row: the first API
	// requestId of that terminal turn. Empty on chat/title rows, which are
	// written once by the turn itself and need no key.
	ReqID string `json:"req_id,omitempty"`
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
	if err := insertUsageTx(tx, evs); err != nil {
		return err
	}
	return tx.Commit()
}

func insertUsageTx(tx *sql.Tx, evs []UsageEvent) error {
	metered := map[string]bool{}
	for _, e := range evs {
		if e.TS.IsZero() {
			e.TS = time.Now()
		}
		if e.Kind == "" {
			e.Kind = UsageKindChat
		}
		snapshot, err := priceJSON(e.Price)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO usage_events
			(ts, user, session_id, thread_id, turn_id, agent, account_id, model, kind,
			 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			 cost_micro_usd, duration_ms, wall_ms, ttft_ms, provider, req_id, raw, price_snapshot)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			usageTimestamp(e.TS), e.User, e.SessionID, e.ThreadID, e.TurnID,
			e.Agent, e.AccountID, e.Model, e.Kind,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens,
			e.CostMicroUSD, e.DurationMS, e.WallMS, e.TTFTMs, e.Provider, e.ReqID, e.Raw, snapshot)
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
	return nil
}

// UpsertTerminalUsage records spend the user drove by hand in the terminal,
// backfilled from the CLI's own transcript. It is deliberately NOT InsertUsage:
//
//   - It never charges. The rows land minutes after the fact, in a batch, with a
//     price we derived ourselves rather than one the provider quoted — silently
//     draining a balance on that basis would be unexplainable to the user. The
//     guard against terminal spend is the balance check that blocks opening a
//     terminal at all (see the quota section in AGENTS.md), not a debit here.
//   - It upserts on ReqID. The scanner re-reads whole transcript files, and a
//     turn that was still in flight last sweep comes back with more tokens on
//     it, so the same key has to update in place rather than pile up.
//
// ReqID must be non-empty; a row without one would fall out of the partial
// unique index and duplicate on the next sweep.
func (s *Store) UpsertTerminalUsage(evs ...UsageEvent) error {
	if len(evs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range evs {
		if e.ReqID == "" {
			return fmt.Errorf("terminal usage %s: 缺 req_id", e.SessionID)
		}
		if e.TS.IsZero() {
			e.TS = time.Now()
		}
		// Read under the same transaction as the upsert: concurrent scanners
		// cannot replace the first observed price snapshot with a later table.
		var oldJSON string
		var oldCost, inTok, outTok, readTok, writeTok int64
		err := tx.QueryRow("SELECT price_snapshot,cost_micro_usd,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens FROM usage_events WHERE req_id=?", e.ReqID).Scan(&oldJSON, &oldCost, &inTok, &outTok, &readTok, &writeTok)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			if oldJSON != "" {
				var original PriceSnapshot
				if err := json.Unmarshal([]byte(oldJSON), &original); err != nil {
					return err
				}
				e.Price = &original
				e.CostMicroUSD = original.Cost(e)
			} else {
				// A pre-snapshot row has no recoverable historical price. Keep its
				// cost when unchanged; growing legacy turns retain legacy labeling.
				e.Price = nil
				if e.InputTokens == inTok && e.OutputTokens == outTok && e.CacheReadTokens == readTok && e.CacheWriteTokens == writeTok {
					e.CostMicroUSD = oldCost
				}
			}
		}
		snapshot, err := priceJSON(e.Price)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO usage_events
			(ts, user, session_id, thread_id, turn_id, agent, account_id, model, kind,
			 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			 cost_micro_usd, duration_ms, wall_ms, ttft_ms, provider, req_id, raw, price_snapshot)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			-- 冲突目标必须把部分索引的 WHERE 原样带上，否则 SQLite 认不出
			-- 这条唯一约束，直接报 "does not match any PRIMARY KEY or UNIQUE"。
			ON CONFLICT(req_id) WHERE req_id != '' DO UPDATE SET
				ts = excluded.ts, model = excluded.model,
				input_tokens = excluded.input_tokens, output_tokens = excluded.output_tokens,
				cache_read_tokens = excluded.cache_read_tokens,
				cache_write_tokens = excluded.cache_write_tokens,
				cost_micro_usd = excluded.cost_micro_usd, price_snapshot = excluded.price_snapshot`,
			usageTimestamp(e.TS), e.User, e.SessionID, e.ThreadID, e.TurnID,
			e.Agent, e.AccountID, e.Model, UsageKindTerminal,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens,
			e.CostMicroUSD, e.DurationMS, e.WallMS, e.TTFTMs, e.Provider, e.ReqID, e.Raw, snapshot); err != nil {
			return fmt.Errorf("upsert terminal usage %s/%s: %w", e.SessionID, e.ReqID, err)
		}
	}
	return tx.Commit()
}

const usageCols = `id, ts, user, session_id, thread_id, turn_id, agent, account_id, model, kind,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	cost_micro_usd, duration_ms, wall_ms, ttft_ms, provider, req_id, raw, price_snapshot`

// Fixed-width UTC text preserves chronological ordering in SQLite's ts index.
func usageTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// UsageFilter narrows a usage query. The zero value matches everything.
type UsageFilter struct {
	User      string    // empty: every user
	SessionID string    // empty: every session
	Kind      string    // empty: every kind
	Agent     string    // empty: every agent
	Model     string    // empty: every model
	Since     time.Time // zero: no lower bound (inclusive)
	Until     time.Time // zero: no upper bound (exclusive)
	Limit     int       // <= 0: no limit
	Offset    int       // rows to skip; only meaningful with Limit
	Asc       bool      // oldest first; the zero value keeps the newest-first default
}

// where renders the filter as a SQL predicate plus its arguments. Every usage
// query goes through it so a paged list, its total and its sums can never
// disagree about what the filter meant.
func (f UsageFilter) where() (string, []any) {
	q := " WHERE 1=1"
	var args []any
	add := func(clause string, v any) {
		q += clause
		args = append(args, v)
	}
	if f.User != "" {
		add(" AND user = ?", f.User)
	}
	if f.SessionID != "" {
		add(" AND session_id = ?", f.SessionID)
	}
	if f.Kind != "" {
		add(" AND kind = ?", f.Kind)
	}
	if f.Agent != "" {
		add(" AND agent = ?", f.Agent)
	}
	if f.Model != "" {
		add(" AND model = ?", f.Model)
	}
	if !f.Since.IsZero() {
		add(" AND ts >= ?", usageTimestamp(f.Since))
	}
	if !f.Until.IsZero() {
		add(" AND ts < ?", usageTimestamp(f.Until))
	}
	return q, args
}

// ListUsage returns matching usage rows, newest first — or oldest first when
// f.Asc is set. id breaks ties in the same direction as ts, so two rows written
// in the same millisecond keep a stable relative order and OFFSET paging can
// never show one of them twice or skip it.
func (s *Store) ListUsage(f UsageFilter) []UsageEvent {
	where, args := f.where()
	dir := "DESC"
	if f.Asc {
		dir = "ASC"
	}
	q := "SELECT " + usageCols + " FROM usage_events" + where + " ORDER BY ts " + dir + ", id " + dir
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
		if f.Offset > 0 {
			q += " OFFSET ?"
			args = append(args, f.Offset)
		}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []UsageEvent
	for rows.Next() {
		var e UsageEvent
		var ts, snapshot string
		if err := rows.Scan(&e.ID, &ts, &e.User, &e.SessionID, &e.ThreadID, &e.TurnID,
			&e.Agent, &e.AccountID, &e.Model, &e.Kind,
			&e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheWriteTokens,
			&e.CostMicroUSD, &e.DurationMS, &e.WallMS, &e.TTFTMs, &e.Provider,
			&e.ReqID, &e.Raw, &snapshot); err != nil {
			continue
		}
		if snapshot != "" {
			_ = json.Unmarshal([]byte(snapshot), &e.Price)
		}
		e.TS, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, e)
	}
	return out
}

// UsageTotals is the whole filtered set summed up, independent of paging —
// the numbers a detail page shows above a single page of rows.
//
// Turns counts distinct turn_id, because one turn lands as several rows when
// Claude splits it per model. DurationMS/TTFTMs are turn-level and repeated on
// every row of the turn, so they are deliberately absent: summing them here
// would multiply by the number of models the turn touched.
type UsageTotals struct {
	Rows             int   `json:"rows"`
	Turns            int   `json:"turns"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	CostMicroUSD     int64 `json:"cost_micro_usd"`
}

// SumUsage totals every row the filter matches. Limit/Offset are ignored: the
// summary describes the filter, not the page.
func (s *Store) SumUsage(f UsageFilter) UsageTotals {
	where, args := f.where()
	var t UsageTotals
	row := s.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT turn_id),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(cost_micro_usd),0) FROM usage_events`+where, args...)
	if err := row.Scan(&t.Rows, &t.Turns, &t.InputTokens, &t.OutputTokens,
		&t.CacheReadTokens, &t.CacheWriteTokens, &t.CostMicroUSD); err != nil {
		return UsageTotals{}
	}
	return t
}

// UsageFacets are the distinct values present in the filtered set, for
// populating the detail page's filter dropdowns with what actually occurred
// rather than a hardcoded list that drifts as models come and go.
type UsageFacets struct {
	Users  []string `json:"users"`
	Agents []string `json:"agents"`
	Models []string `json:"models"`
}

// FacetsUsage collects those distinct values.
//
// scopeUser is the hard visibility boundary and is re-applied to every column:
// pass a regular user's own name so their dropdowns can never list other
// users, and "" for an admin who may see everyone. It is deliberately separate
// from f.User, which is the selectable filter and gets relaxed below.
func (s *Store) FacetsUsage(f UsageFilter, scopeUser string) UsageFacets {
	// Facets describe the axes you can still filter by, so each column ignores
	// its own filter — otherwise picking one model collapses the model list to
	// that one value and there is no way back without a reset.
	out := UsageFacets{Users: []string{}, Agents: []string{}, Models: []string{}}
	distinct := func(col string, f UsageFilter) []string {
		where, args := f.where()
		rows, err := s.db.Query("SELECT DISTINCT "+col+" FROM usage_events"+where+
			" AND "+col+" != '' ORDER BY "+col, args...)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var vals []string
		for rows.Next() {
			var v string
			if rows.Scan(&v) == nil {
				vals = append(vals, v)
			}
		}
		return vals
	}
	relax := func(clear func(*UsageFilter)) UsageFilter {
		g := f
		clear(&g)
		if scopeUser != "" {
			g.User = scopeUser
		}
		return g
	}
	out.Users = append(out.Users, distinct("user", relax(func(g *UsageFilter) { g.User = "" }))...)
	out.Agents = append(out.Agents, distinct("agent", relax(func(g *UsageFilter) { g.Agent = "" }))...)
	out.Models = append(out.Models, distinct("model", relax(func(g *UsageFilter) { g.Model = "" }))...)
	return out
}
