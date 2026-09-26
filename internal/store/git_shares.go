package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type GitShare struct {
	User  string `json:"user"`
	Write bool   `json:"write"`
}

// GitConnectionFor resolves execution permission. Administrative role alone
// never grants access to someone else's private credential.
func (s *Store) GitConnectionFor(actor, id string) (GitConnection, error) {
	c, err := scanGitConnection(s.db.QueryRow("SELECT "+gitConnectionCols+" FROM git_connections WHERE id=?", id))
	if err != nil {
		return c, err
	}
	c.Actor = actor
	c.Managed = c.Owner == actor
	if c.Owner == actor {
		return c, nil
	}
	var write bool
	err = s.db.QueryRow(`SELECT sh.can_write FROM git_connection_shares sh JOIN users owner ON owner.name=? AND owner.role='admin' WHERE sh.connection_id=? AND sh.user=?`, c.Owner, id, actor).Scan(&write)
	if err != nil {
		return GitConnection{}, sql.ErrNoRows
	}
	if c.AuthType == "oauth" {
		return GitConnection{}, sql.ErrNoRows
	}
	c.ReadOnly = c.ReadOnly || !write
	return c, nil
}
func (s *Store) GitConnectionsFor(actor string) ([]GitConnection, error) {
	rows, err := s.db.Query(`SELECT id FROM git_connections WHERE owner=? OR id IN (SELECT connection_id FROM git_connection_shares WHERE user=?) ORDER BY created_at,id`, actor, actor)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []GitConnection{}
	for _, id := range ids {
		c, err := s.GitConnectionFor(actor, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		c.Secret = nil
		out = append(out, c)
	}
	return out, nil
}
func (s *Store) GitShares(owner, id string) ([]GitShare, int64, error) {
	c, err := s.GitConnection(owner, id)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query("SELECT user,can_write FROM git_connection_shares WHERE connection_id=? ORDER BY user", id)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []GitShare{}
	for rows.Next() {
		var share GitShare
		if err = rows.Scan(&share.User, &share.Write); err != nil {
			return nil, 0, err
		}
		out = append(out, share)
	}
	return out, c.Revision, rows.Err()
}
func (s *Store) SetGitShares(owner, id string, revision int64, shares []GitShare) error {
	if len(shares) > 500 {
		return fmt.Errorf("共享用户数量超过 500")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	if err = tx.QueryRow("SELECT role FROM users WHERE name=?", owner).Scan(&role); err != nil || role != RoleAdmin {
		return sql.ErrNoRows
	}
	c, err := scanGitConnection(tx.QueryRow("SELECT "+gitConnectionCols+" FROM git_connections WHERE owner=? AND id=?", owner, id))
	if err != nil {
		return err
	}
	if c.Revision != revision {
		return ErrGitConflict
	}
	if c.AuthType == "oauth" {
		return fmt.Errorf("共享连接需使用专用 Token 或 SSH 服务账号，个人 OAuth 不开放共享")
	}
	seen := map[string]bool{}
	for _, share := range shares {
		if share.User == owner || seen[share.User] {
			return fmt.Errorf("不能重复授权或授权给连接属主")
		}
		seen[share.User] = true
		var exists int
		if err = tx.QueryRow("SELECT COUNT(*) FROM users WHERE name=?", share.User).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return fmt.Errorf("指定用户不存在")
		}
	}
	if _, err = tx.Exec("DELETE FROM git_connection_shares WHERE connection_id=?", id); err != nil {
		return err
	}
	for _, share := range shares {
		if _, err = tx.Exec("INSERT INTO git_connection_shares(connection_id,user,can_write) VALUES(?,?,?)", id, share.User, share.Write); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE git_connections SET revision=revision+1,updated_at=? WHERE id=?", time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	// Remove inaccessible defaults, leaving repository bindings visible as broken
	// until explicitly rebound so operations cannot silently switch identities.
	if _, err = tx.Exec(`DELETE FROM git_defaults WHERE connection_id=? AND user!=? AND user NOT IN (SELECT user FROM git_connection_shares WHERE connection_id=?)`, id, owner, id); err != nil {
		return err
	}
	if err = gitAudit(tx, owner, "", "", id, "connection.share", "", "success"); err != nil {
		return err
	}
	return tx.Commit()
}

const gitCanSelectSQL = `SELECT COUNT(*) FROM git_connections c WHERE c.id=? AND c.enabled=1 AND (c.owner=? OR (c.auth_type!='oauth' AND EXISTS (SELECT 1 FROM git_connection_shares sh JOIN users owner ON owner.name=c.owner AND owner.role='admin' WHERE sh.connection_id=c.id AND sh.user=?)))`
