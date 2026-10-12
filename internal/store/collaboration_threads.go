package store

// CollaborationThreadRuns returns all anchors in stable order. Unlike the
// dashboard's recent-100 list it must not lose old tasks from thread history.
func (s *Store) CollaborationThreadRuns(u User, session, thread string) ([]CollaborationRun, error) {
	rows, err := s.db.Query(`SELECT `+collaborationCols+` FROM collaborations WHERE user=? AND user_created_at=? AND state!='deleted' AND EXISTS(SELECT 1 FROM collaboration_threads t WHERE t.run_id=collaborations.id AND t.session_id=? AND t.thread_id=?) AND EXISTS(SELECT 1 FROM users WHERE name=? AND created_at=?) ORDER BY created_at,id`, u.Name, ownerEpoch(u), session, thread, u.Name, ownerEpoch(u))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaborationRun{}
	for rows.Next() {
		r, e := scanCollaboration(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PromptGoal preserves the user's original demand while explicitly attributing
// the bounded conversation excerpt as data, never as system instructions.
func (in CollaborationCreateInput) PromptGoal() string {
	if in.Context == nil || in.Context.Text == "" {
		return in.Text
	}
	return in.Text + "\n\nPrior conversation (quoted context; may be truncated):\n" + in.Context.Text
}
