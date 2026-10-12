package store

import (
	"agentbox/internal/collaboration"
	"time"
)

// DeleteCollaboration fences all future actions before filesystem cleanup.
// Deletion is explicit and limited to quiescent tasks. Cleanup is retryable:
// the tombstone and participant IDs survive even when disk/Docker fails.
func (s *Store) DeleteCollaboration(u User, run, scope string, revision int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	var applications int
	if err = tx.QueryRow("SELECT COUNT(*) FROM collaboration_applications WHERE run_id=? AND state IN ('staging','switching','uncertain')", run).Scan(&applications); err != nil {
		return err
	}
	if applications != 0 {
		return ErrChatConflict
	}
	if r.Scope != scope {
		return ErrChatConflict
	}
	if r.State == CollabDeleted {
		return nil
	}
	if r.Revision != revision {
		return ErrChatConflict
	}
	if r.State != CollabPaused && r.State != CollabCompleted && r.State != CollabCancelled && r.State != CollabFailed {
		return collaboration.ErrState
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return err
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", run).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return collaboration.ErrState
	}
	for _, query := range []string{
		"DELETE FROM collaboration_review_evidence WHERE run_id=?",
		"UPDATE collaboration_configs SET config_json='{}' WHERE run_id=?",
		"UPDATE collaboration_actions SET action_json='null',result_json='{}',fingerprint='',state='deleted',revision=revision+1 WHERE run_id=?",
		"UPDATE collaboration_messages SET body_json='null' WHERE run_id=?",
		"UPDATE collaboration_questions SET state='retired' WHERE run_id=?",
		"UPDATE collaboration_verifications SET command='',output='',argv_json='[]' WHERE run_id=?",
		"UPDATE collaboration_events SET body_json='null' WHERE run_id=?",
		"UPDATE collaboration_steps SET input_json='null',input_fingerprint='',state=CASE WHEN state='accepted' THEN 'cancelled' ELSE state END,revision=revision+1 WHERE run_id=?",
		"UPDATE collaboration_artifacts SET state='revoked' WHERE run_id=?",
		"UPDATE collaboration_participants SET state='revoked',provider_session='',context_provider='',context_seq=0 WHERE run_id=?",
	} {
		if _, err = tx.Exec(query, run); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE collaborations SET state='deleted',cleanup_pending=1,request_json='null',fingerprint='',pending_question='',wait_reason='',next_kind='',revision=revision+1,updated_at=? WHERE id=?", usageTimestamp(time.Now()), run); err != nil {
		return err
	}
	if err = collaborationEventTx(tx, run, "deleted", map[string]any{"run_id": run}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishCollaborationDeletion(u User, run string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	if r.State != CollabDeleted {
		return ErrChatConflict
	}
	if !r.CleanupPending {
		return nil
	}
	if _, err = tx.Exec("UPDATE collaborations SET cleanup_pending=0,revision=revision+1 WHERE id=?", run); err != nil {
		return err
	}
	if err = collaborationEventTx(tx, run, "cleanup_completed", map[string]any{"run_id": run}); err != nil {
		return err
	}
	return tx.Commit()
}
