package store

import (
	"encoding/hex"
	"encoding/json"
	"strings"

	"agentbox/internal/collaboration"
)

func (s *Store) PinCollaborationImage(u User, run string, revision int64, image string) error {
	if !strings.HasPrefix(image, "sha256:") || len(image) != 71 {
		return ErrChatInvalid
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(image, "sha256:")); err != nil {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	if r.State != CollabPreparing || r.Revision != revision {
		return ErrChatConflict
	}
	var prior string
	if err = tx.QueryRow("SELECT image_id FROM collaborations WHERE id=?", run).Scan(&prior); err != nil {
		return err
	}
	if prior != "" && prior != image {
		return ErrChatConflict
	}
	if _, err = tx.Exec("UPDATE collaborations SET image_id=? WHERE id=?", image, run); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CollaborationImage(u User, run string) (string, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return "", err
	}
	var image string
	err := s.db.QueryRow("SELECT image_id FROM collaborations WHERE id=?", run).Scan(&image)
	return image, err
}

type CollaborationWork struct {
	Run        CollaborationRun
	Step       CollaborationStep
	Input      CollaborationStepInput
	Binding    CollaborationBinding
	Session    Session
	SnapshotID string
}

// CollaborationWork reads a single immutable pending/owned step. It grants no
// execution rights: ClaimCollaborationStep must still win the durable CAS.
func (s *Store) CollaborationWork(u User, run string) (CollaborationWork, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationWork{}, err
	}
	defer tx.Rollback()
	var w CollaborationWork
	w.Run, err = collaborationRunTx(tx, u, run)
	if err != nil {
		return w, err
	}
	w.Step, err = scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE run_id=? AND state IN ('accepted','starting','running','finalizing','uncertain')", run))
	if err != nil {
		return w, err
	}
	var raw, fingerprint string
	if err = tx.QueryRow("SELECT input_json,input_fingerprint FROM collaboration_steps WHERE id=?", w.Step.ID).Scan(&raw, &fingerprint); err != nil {
		return w, err
	}
	if err = json.Unmarshal([]byte(raw), &w.Input); err != nil {
		return w, err
	}
	fp, _, err := collaborationFingerprint(w.Input)
	if err != nil {
		return w, err
	}
	if fp != fingerprint {
		return w, ErrChatConflict
	}
	var cfg CollaborationConfig
	if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=?", run, w.Step.ConfigVersion).Scan(&raw); err != nil {
		return w, err
	}
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return w, err
	}
	var ok bool
	w.Binding, ok = cfg.Roles.Binding(collaboration.Role(w.Step.Role))
	if !ok {
		return w, ErrChatInvalid
	}
	var sessionID, providerSession string
	if err = tx.QueryRow("SELECT session_id,provider_session FROM collaboration_participants WHERE run_id=? AND role=? AND generation=? AND config_version=? AND state='active'", run, w.Step.Role, w.Step.ParticipantGeneration, w.Step.ConfigVersion).Scan(&sessionID, &providerSession); err != nil {
		return w, err
	}
	w.Session, err = scanSession(tx.QueryRow("SELECT "+sessionCols+" FROM sessions WHERE id=? AND user=?", sessionID, u.Name))
	if err != nil {
		return w, err
	}
	if w.Session.Agent != w.Binding.AgentType || w.Session.AccountID != w.Binding.AccountID {
		return w, ErrChatConflict
	}
	if w.Input.Context != nil && (w.Session.ChatSession != providerSession || (w.Input.Context.ProviderSession != "" && w.Input.Context.ProviderSession != providerSession)) {
		// An incremental prompt is meaningful only in the exact native thread
		// whose successful receipt established its starting boundary.
		return w, ErrChatConflict
	}
	err = tx.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", run).Scan(&w.SnapshotID)
	return w, err
}

func (s *Store) SaveCollaborationProviderSession(u User, run, step string, stepRevision int64, id string) error {
	if !validChatKey(id) {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	st, err := scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE id=? AND run_id=?", step, run))
	if err != nil {
		return err
	}
	if st.State != "running" || st.Revision != stepRevision || st.ConfigVersion != r.ConfigVersion {
		return ErrChatConflict
	}
	var sid, existing string
	if err = tx.QueryRow("SELECT session_id,provider_session FROM collaboration_participants WHERE run_id=? AND role=? AND generation=? AND state='active'", run, st.Role, st.ParticipantGeneration).Scan(&sid, &existing); err != nil {
		return err
	}
	if existing != "" && existing != id {
		return ErrChatConflict
	}
	sess, err := scanSession(tx.QueryRow("SELECT "+sessionCols+" FROM sessions WHERE id=?", sid))
	if err != nil {
		return err
	}
	if sess.ChatSession != "" && sess.ChatSession != id {
		return ErrChatConflict
	}
	sess.ChatSession = id
	if err = s.put(tx, sess); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaboration_participants SET provider_session=? WHERE run_id=? AND role=? AND generation=?", id, run, st.Role, st.ParticipantGeneration); err != nil {
		return err
	}
	return tx.Commit()
}

// Internal scheduler enumeration. Returning errors preserves the difference
// between a genuinely empty queue and a broken database.
func (s *Store) PendingCollaborationRuns() ([]CollaborationRun, error) {
	rows, err := s.db.Query("SELECT " + collaborationCols + " FROM collaborations WHERE state IN ('accepted','planning','executing','reviewing','waiting_answer','cancelling') ORDER BY created_at LIMIT 100")
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

func (s *Store) FailCollaborationPreparation(u User, run string, revision int64, reason string) (CollaborationRun, error) {
	switch reason {
	case "project_invalid", "snapshot_failed", "credentials_failed", "authorization_revoked", "image_unavailable", "artifacts_missing", "preparation_failed":
	default:
		return CollaborationRun{}, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return r, err
	}
	if r.State != CollabPreparing || r.Revision != revision {
		return r, ErrChatConflict
	}
	r.State, r.WaitReason = CollabFailed, reason
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, err
	}
	return r, tx.Commit()
}
