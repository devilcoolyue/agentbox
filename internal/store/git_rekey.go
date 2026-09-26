package store

import "bytes"

// RekeyGitCredentials rewrites all connection secrets in one transaction. The
// transform reads authenticated identities but cannot change account metadata.
// Call only in offline maintenance under the instance's exclusive data lock.
func (s *Store) RekeyGitCredentials(transform func(GitConnection) ([]byte, error)) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT " + gitConnectionCols + " FROM git_connections ORDER BY id")
	if err != nil {
		return 0, err
	}
	items := []GitConnection{}
	for rows.Next() {
		item, err := scanGitConnection(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, item := range items {
		secret, err := transform(item)
		if err != nil {
			return 0, err
		}
		if bytes.Equal(secret, item.Secret) {
			continue
		}
		if _, err = tx.Exec("UPDATE git_connections SET secret=? WHERE id=?", secret, item.ID); err != nil {
			return 0, err
		}
	}
	return len(items), tx.Commit()
}
