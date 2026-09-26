package store

import (
	"agentbox/internal/gitaccess"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrGitConflict = errors.New("Git 配置已变更，请刷新后重试")
var ErrGitInUse = errors.New("连接仍被仓库或默认设置引用，请先解除绑定；可停用连接立即阻止后续使用")
var ErrGitLimit = errors.New("Git 连接数量已达上限（100）")

type GitConnection struct {
	Actor           string                  `json:"-"`
	Managed         bool                    `json:"managed"`
	PublicKey       string                  `json:"public_key,omitempty"`
	HostFingerprint string                  `json:"host_fingerprint,omitempty"`
	Network         gitaccess.NetworkPolicy `json:"network"`
	OAuthAppID      string                  `json:"oauth_app_id,omitempty"`

	ID        string `json:"id"`
	Owner     string `json:"owner"`
	Label     string `json:"label"`
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	AuthType  string `json:"auth_type"`
	Username  string `json:"username"`
	Secret    []byte `json:"-"`
	ReadOnly  bool   `json:"read_only"`
	Enabled   bool   `json:"enabled"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (c GitConnection) AssociatedData() []byte {
	parts := []string{"agentbox-git-v1", c.ID, c.Owner, c.Provider, c.BaseURL, c.AuthType, c.Username}
	if c.Network.Configured() {
		parts = append(parts, c.Network.Identity())
	}
	data, _ := json.Marshal(parts)
	return data
}

const gitConnectionCols = "id,owner,label,provider,base_url,auth_type,username,secret,read_only,enabled,revision,created_at,updated_at,network"

func scanGitConnection(row interface{ Scan(...any) error }) (GitConnection, error) {
	var c GitConnection
	var network string
	err := row.Scan(&c.ID, &c.Owner, &c.Label, &c.Provider, &c.BaseURL, &c.AuthType, &c.Username, &c.Secret, &c.ReadOnly, &c.Enabled, &c.Revision, &c.CreatedAt, &c.UpdatedAt, &network)
	if err == nil {
		err = json.Unmarshal([]byte(network), &c.Network)
	}
	return c, err
}
func (s *Store) GitConnection(owner, id string) (GitConnection, error) {
	return scanGitConnection(s.db.QueryRow("SELECT "+gitConnectionCols+" FROM git_connections WHERE owner=? AND id=?", owner, id))
}
func (s *Store) GitConnections(owner string) ([]GitConnection, error) {
	rows, err := s.db.Query("SELECT "+gitConnectionCols+" FROM git_connections WHERE owner=? ORDER BY created_at,id", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitConnection{}
	for rows.Next() {
		c, err := scanGitConnection(rows)
		if err != nil {
			return nil, err
		}
		c.Secret = nil
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) GitConnectionCount() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM git_connections").Scan(&n)
	return n, err
}
func (s *Store) SaveGitConnection(c GitConnection, expected int64) (GitConnection, error) {
	return s.saveGitConnection(c, expected, "")
}

// Verify the original login in the same transaction as credential publication;
// deletion/recreation of a username cannot inherit a late OAuth callback.
func (s *Store) SaveGitConnectionWithLogin(c GitConnection, expected int64, token string) (GitConnection, error) {
	return s.saveGitConnection(c, expected, token)
}
func (s *Store) saveGitConnection(c GitConnection, expected int64, token string) (GitConnection, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var ownerExists int
	if err = tx.QueryRow("SELECT COUNT(*) FROM users WHERE name=?", c.Owner).Scan(&ownerExists); err != nil {
		return c, err
	}
	if ownerExists != 1 {
		return c, sql.ErrNoRows
	}
	if token != "" {
		var loggedIn int
		if err = tx.QueryRow("SELECT COUNT(*) FROM tokens WHERE token=? AND user=?", token, c.Owner).Scan(&loggedIn); err != nil {
			return c, err
		}
		if loggedIn != 1 {
			return c, sql.ErrNoRows
		}
	}
	network, _ := json.Marshal(c.Network)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	action := "connection.update"
	if expected == 0 {
		var count int
		if err = tx.QueryRow("SELECT COUNT(*) FROM git_connections WHERE owner=?", c.Owner).Scan(&count); err != nil {
			return c, err
		}
		if count >= 100 {
			return c, ErrGitLimit
		}
		c.Revision = 1
		c.CreatedAt = now
		action = "connection.create"
		_, err = tx.Exec("INSERT INTO git_connections ("+gitConnectionCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", c.ID, c.Owner, c.Label, c.Provider, c.BaseURL, c.AuthType, c.Username, c.Secret, c.ReadOnly, c.Enabled, c.Revision, c.CreatedAt, now, string(network))
	} else {
		var res sql.Result
		res, err = tx.Exec(`UPDATE git_connections SET label=?,secret=?,read_only=?,enabled=?,revision=revision+1,updated_at=? WHERE id=? AND owner=? AND revision=? AND provider=? AND base_url=? AND auth_type=? AND username=? AND network=?`, c.Label, c.Secret, c.ReadOnly, c.Enabled, now, c.ID, c.Owner, expected, c.Provider, c.BaseURL, c.AuthType, c.Username, string(network))
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return c, ErrGitConflict
			}
		}
		c.Revision = expected + 1
	}
	if err != nil {
		return c, err
	}
	c.UpdatedAt = now
	if err = gitAudit(tx, c.Owner, "", "", c.ID, action, "", "success"); err != nil {
		return c, err
	}
	if err = tx.Commit(); err != nil {
		return c, err
	}
	c.Secret = nil
	return c, nil
}
func (s *Store) DeleteGitConnection(owner, id string, revision int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRow("SELECT COUNT(*) FROM git_connections WHERE owner=? AND id=? AND revision=?", owner, id, revision).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrGitConflict
	}
	if err = tx.QueryRow("SELECT (SELECT COUNT(*) FROM git_bindings WHERE connection_id=?) + (SELECT COUNT(*) FROM git_defaults WHERE connection_id=?)", id, id).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return ErrGitInUse
	}
	if _, err = tx.Exec("DELETE FROM git_connection_shares WHERE connection_id=?", id); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM git_connections WHERE owner=? AND id=?", owner, id); err != nil {
		return err
	}
	if err = gitAudit(tx, owner, "", "", id, "connection.delete", "", "success"); err != nil {
		return err
	}
	return tx.Commit()
}

func gitAudit(tx *sql.Tx, actor, session, repo, connection, operation, target, result string) error {
	_, err := tx.Exec("INSERT INTO git_operations (ts,actor,session_id,repo,connection_id,operation,target,result,finished_at) VALUES (?,?,?,?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339Nano), actor, session, repo, connection, operation, target, result, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

type GitBinding struct {
	SessionID    string `json:"session_id"`
	Repo         string `json:"repo"`
	Remote       string `json:"remote"`
	URL          string `json:"url"`
	ConnectionID string `json:"connection_id"`
	Revision     int64  `json:"revision"`
}

func (s *Store) GitBindings(session string) ([]GitBinding, error) {
	rows, err := s.db.Query("SELECT session_id,repo,remote,url,connection_id,revision FROM git_bindings WHERE session_id=? ORDER BY repo,remote", session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitBinding{}
	for rows.Next() {
		var b GitBinding
		if err := rows.Scan(&b.SessionID, &b.Repo, &b.Remote, &b.URL, &b.ConnectionID, &b.Revision); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *Store) SaveGitBinding(owner string, b GitBinding, expected int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found int
	if err = tx.QueryRow("SELECT COUNT(*) FROM sessions WHERE id=? AND user=?", b.SessionID, owner).Scan(&found); err != nil {
		return err
	}
	if found != 1 {
		return sql.ErrNoRows
	}
	if b.ConnectionID != "" {
		if err = tx.QueryRow(gitCanSelectSQL, b.ConnectionID, owner, owner).Scan(&found); err != nil {
			return err
		}
		if found != 1 {
			return sql.ErrNoRows
		}
	}
	var revision int64
	err = tx.QueryRow("SELECT revision FROM git_bindings WHERE session_id=? AND repo=? AND remote=?", b.SessionID, b.Repo, b.Remote).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if revision != expected {
		return ErrGitConflict
	}
	if b.ConnectionID == "" {
		_, err = tx.Exec("DELETE FROM git_bindings WHERE session_id=? AND repo=? AND remote=?", b.SessionID, b.Repo, b.Remote)
	} else {
		_, err = tx.Exec(`INSERT INTO git_bindings (session_id,repo,remote,url,connection_id,revision) VALUES (?,?,?,?,?,?) ON CONFLICT(session_id,repo,remote) DO UPDATE SET url=excluded.url,connection_id=excluded.connection_id,revision=excluded.revision`, b.SessionID, b.Repo, b.Remote, b.URL, b.ConnectionID, revision+1)
	}
	if err != nil {
		return err
	}
	if err = gitAudit(tx, owner, b.SessionID, b.Repo, b.ConnectionID, "binding.update", b.Remote, "success"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GitDefault(user, session string) (string, error) {
	var id string
	err := s.db.QueryRow("SELECT connection_id FROM git_defaults WHERE user=? AND session_id=?", user, session).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
func (s *Store) SetGitDefault(user, session, id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if session != "" {
		if err = tx.QueryRow("SELECT COUNT(*) FROM sessions WHERE user=? AND id=?", user, session).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}
	}
	if id != "" {
		if err = tx.QueryRow(gitCanSelectSQL, id, user, user).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}
		_, err = tx.Exec("INSERT INTO git_defaults (user,session_id,connection_id) VALUES (?,?,?) ON CONFLICT(user,session_id) DO UPDATE SET connection_id=excluded.connection_id", user, session, id)
	} else {
		_, err = tx.Exec("DELETE FROM git_defaults WHERE user=? AND session_id=?", user, session)
	}
	if err != nil {
		return err
	}
	if err = gitAudit(tx, user, session, "", id, "default.update", "", "success"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginGitOperation(actor, session, repo, connection, operation, target string) (int64, error) {
	res, err := s.db.Exec("INSERT INTO git_operations (ts,actor,session_id,repo,connection_id,operation,target,result) VALUES (?,?,?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339Nano), actor, session, repo, connection, operation, target, "running")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
func (s *Store) FinishGitOperation(id int64, result string) error {
	_, err := s.db.Exec("UPDATE git_operations SET result=?,finished_at=? WHERE id=? AND result='running'", result, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

// PutWithGitDefault creates the session row and its selected default together.
// The access check is repeated inside the transaction to cover concurrent revoke.
func (s *Store) PutWithGitDefault(sess Session, connection string) error {
	if connection == "" {
		return s.Put(sess)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRow(gitCanSelectSQL, connection, sess.User, sess.User).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	sess.UpdatedAt = time.Now()
	if err = s.put(tx, sess); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO git_defaults (user,session_id,connection_id) VALUES (?,?,?)", sess.User, sess.ID, connection); err != nil {
		return err
	}
	return tx.Commit()
}
