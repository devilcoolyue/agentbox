package store

// InitDefaultModels gives pre-upgrade workspaces a concrete default once.
// Existing snapshots survive settings changes and subsequent server restarts.
func (s *Store) InitDefaultModels(models map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for agent, model := range models {
		if _, err := tx.Exec("UPDATE sessions SET default_model = ? WHERE agent = ? AND default_model = ''", model, agent); err != nil {
			return err
		}
	}
	return tx.Commit()
}
