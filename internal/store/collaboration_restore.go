package store

import (
	"time"
)

// BindCollaborationScope runs during startup, after recovery and before any
// task can run. Keep frozen creation/action inputs for replay and audit; only
// the scope admitting future actions changes with the installation identity.
func (s *Store) BindCollaborationScope(u User, scope string) error {
	if scope == "" || len(scope) > 128 {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT "+collaborationCols+" FROM collaborations WHERE user=? AND user_created_at=? AND current_scope!=? AND state!='deleted'", u.Name, ownerEpoch(u), scope)
	if err != nil {
		return err
	}
	var runs []CollaborationRun
	for rows.Next() {
		r, e := scanCollaboration(rows)
		if e != nil {
			rows.Close()
			return e
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range runs {
		switch r.State {
		case CollabPaused, CollabUncertain, CollabCompleted, CollabCancelled, CollabFailed:
		default:
			return ErrChatConflict
		}
		if _, err = tx.Exec("UPDATE collaborations SET current_scope=? WHERE id=?", scope, r.ID); err != nil {
			return err
		}
		if err = collaborationEventTx(tx, r.ID, "scope_changed", map[string]string{"previous_scope": r.Scope, "scope": scope}); err != nil {
			return err
		}
		r.Scope = scope
		if err = saveCollaborationTx(tx, &r); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RestoreCollaborations belongs exclusively to offline backup restoration into
// a fresh data directory. Old containers still belong to the source instance;
// never attach to or stop them from the restored copy. Full restores require
// new participant generations. System restores lack project files and are
// terminally unavailable; metadata must not advertise downloadable results.
func (s *Store) RestoreCollaborations(full bool) error {
	if err := s.RecoverCollaborations(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !full {
		if _, err = tx.Exec("UPDATE collaboration_applications SET state='failed'"); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec("UPDATE collaboration_applications SET state='uncertain' WHERE state IN ('staging','switching')"); err != nil {
			return err
		}
	}
	rows, err := tx.Query("SELECT " + collaborationCols + " FROM collaborations WHERE state!='deleted'")
	if err != nil {
		return err
	}
	var runs []CollaborationRun
	for rows.Next() {
		r, e := scanCollaboration(rows)
		if e != nil {
			rows.Close()
			return e
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range runs {
		for _, query := range []string{
			"UPDATE sessions SET container_id='',status='stopped',chat_session='' WHERE id IN (SELECT session_id FROM collaboration_participants WHERE run_id=?)",
			"UPDATE collaboration_participants SET state='revoked',stopped_epoch=process_epoch,provider_session='',context_provider='',context_seq=0 WHERE run_id=?",
			"UPDATE collaboration_verifications SET state='failed' WHERE run_id=? AND state IN ('running','uncertain')",
			"UPDATE collaboration_steps SET state='cancelled',revision=revision+1 WHERE run_id=? AND state='accepted'",
		} {
			if _, err = tx.Exec(query, r.ID); err != nil {
				return err
			}
		}
		if !full {
			if _, err = tx.Exec("UPDATE collaboration_artifacts SET state='missing' WHERE run_id=? AND state='ready'", r.ID); err != nil {
				return err
			}
			r.State, r.WaitReason = CollabFailed, "artifacts_missing"
		} else if r.State != CollabCompleted && r.State != CollabCancelled && r.State != CollabFailed {
			if r.State != CollabUncertain {
				r.State = CollabPaused
			}
			r.WaitReason = "instance_restored"
		}
		r.ResumeState, r.NextKind = "", ""
		if err = collaborationEventTx(tx, r.ID, "instance_restored", map[string]any{"full": full, "runtime_detached": true, "at": usageTimestamp(time.Now())}); err != nil {
			return err
		}
		if err = saveCollaborationTx(tx, &r); err != nil {
			return err
		}
	}
	return tx.Commit()
}
