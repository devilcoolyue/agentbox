package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"agentbox/internal/collaboration"
)

const CollaborationArtifactBudget int64 = 512 << 20

type CollaborationArtifact struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	StepID     string `json:"step_id"`
	Kind       string `json:"kind"`
	StorageKey string `json:"-"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	State      string `json:"state"`
	CreatedAt  string `json:"created_at"`
}

const collaborationArtifactCols = "id,run_id,step_id,kind,storage_key,sha256,size_bytes,state,created_at"

func (s *Store) CollaborationLatestArtifact(u User, run string) (CollaborationArtifact, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return CollaborationArtifact{}, err
	}
	var id string
	if err := s.db.QueryRow("SELECT snapshot_id FROM collaborations WHERE id=?", run).Scan(&id); err != nil {
		return CollaborationArtifact{}, err
	}
	return s.CollaborationArtifact(u, run, id)
}

func scanCollaborationArtifact(row interface{ Scan(...any) error }) (CollaborationArtifact, error) {
	var a CollaborationArtifact
	err := row.Scan(&a.ID, &a.RunID, &a.StepID, &a.Kind, &a.StorageKey, &a.SHA256, &a.SizeBytes, &a.State, &a.CreatedAt)
	return a, err
}

// RegisterCollaborationSnapshot accepts ONLY the result of a successful
// PublishSnapshot + VerifySnapshot by the runtime after writers stop. Artifact
// storage keys are synthesized here, never accepted from model/HTTP input.
// Filesystem and DB commits are separate: unregistered files remain private
// orphans; only this durable ready row makes a snapshot eligible for handoff.
func (s *Store) RegisterCollaborationSnapshot(u User, run string, runRevision int64, stepID string, stepRevision int64, snapshot collaboration.Snapshot) (CollaborationArtifact, error) {
	if !collaboration.ValidID(snapshot.ID) || snapshot.Manifest.Version != 1 || len(snapshot.ManifestSHA256) != 64 || snapshot.Manifest.Bytes < 0 || snapshot.Manifest.Bytes > 1<<40 {
		return CollaborationArtifact{}, ErrChatInvalid
	}
	if _, err := hex.DecodeString(snapshot.ManifestSHA256); err != nil {
		return CollaborationArtifact{}, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationArtifact{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationArtifact{}, err
	}
	prior, err := scanCollaborationArtifact(tx.QueryRow("SELECT "+collaborationArtifactCols+" FROM collaboration_artifacts WHERE run_id=? AND id=?", run, snapshot.ID))
	if err == nil {
		if prior.State != "ready" || r.State == CollabDeleted {
			return prior, ErrChatGone
		}
		if prior.SHA256 != snapshot.ManifestSHA256 || prior.SizeBytes != snapshot.Manifest.Bytes || prior.StepID != stepID {
			return prior, ErrChatConflict
		}
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CollaborationArtifact{}, err
	}
	if r.Revision != runRevision {
		return CollaborationArtifact{}, ErrChatConflict
	}
	var used int64
	if err = tx.QueryRow("SELECT COALESCE(SUM(size_bytes),0) FROM collaboration_artifacts WHERE run_id=? AND state='ready'", run).Scan(&used); err != nil {
		return CollaborationArtifact{}, err
	}
	if snapshot.Manifest.Bytes > CollaborationArtifactBudget-used {
		return CollaborationArtifact{}, collaboration.ErrInvalid
	}
	kind := "baseline"
	if stepID == "" {
		if r.State != CollabPreparing || stepRevision != 0 {
			return CollaborationArtifact{}, collaboration.ErrState
		}
	} else {
		kind = "snapshot"
		step, err := scanCollaborationStep(tx.QueryRow("SELECT "+collaborationStepCols+" FROM collaboration_steps WHERE id=? AND run_id=?", stepID, run))
		if err != nil {
			return CollaborationArtifact{}, err
		}
		if step.Revision != stepRevision || step.State != "finalizing" || step.ConfigVersion != r.ConfigVersion || step.Role != "executor" || r.State != CollabExecuting {
			return CollaborationArtifact{}, ErrChatConflict
		}
	}
	a := CollaborationArtifact{ID: snapshot.ID, RunID: run, StepID: stepID, Kind: kind, StorageKey: "snapshots/" + snapshot.ID, SHA256: snapshot.ManifestSHA256, SizeBytes: snapshot.Manifest.Bytes, State: "ready", CreatedAt: usageTimestamp(time.Now())}
	if _, err = tx.Exec("INSERT INTO collaboration_artifacts("+collaborationArtifactCols+") VALUES(?,?,?,?,?,?,?,?,?)", a.ID, a.RunID, a.StepID, a.Kind, a.StorageKey, a.SHA256, a.SizeBytes, a.State, a.CreatedAt); err != nil {
		return a, err
	}
	if stepID == "" {
		if _, err = tx.Exec("UPDATE collaborations SET snapshot_id=? WHERE id=?", a.ID, run); err != nil {
			return a, err
		}
	}
	if err = collaborationEventTx(tx, run, "artifact", a); err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) CollaborationArtifact(u User, run, id string) (CollaborationArtifact, error) {
	if !collaboration.ValidID(id) {
		return CollaborationArtifact{}, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationArtifact{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationArtifact{}, err
	}
	if r.State == CollabDeleted {
		return CollaborationArtifact{}, ErrChatGone
	}
	a, err := scanCollaborationArtifact(tx.QueryRow("SELECT "+collaborationArtifactCols+" FROM collaboration_artifacts WHERE run_id=? AND id=? AND state='ready'", run, id))
	return a, err
}

func (s *Store) CollaborationBaselineArtifact(u User, run string) (CollaborationArtifact, error) {
	if _, err := s.Collaboration(u, run); err != nil {
		return CollaborationArtifact{}, err
	}
	return scanCollaborationArtifact(s.db.QueryRow("SELECT "+collaborationArtifactCols+" FROM collaboration_artifacts WHERE run_id=? AND kind='baseline' AND state='ready' ORDER BY created_at,id LIMIT 1", run))
}

// RegisterCollaborationInspection publishes a stopped uncertain work tree for
// human inspection. It never substitutes for a completed implementation or
// changes the reviewed/deliverable snapshot. Any concurrent control wins CAS.
func (s *Store) RegisterCollaborationInspection(u User, run string, revision int64, snapshot collaboration.Snapshot) (CollaborationArtifact, error) {
	if !collaboration.ValidID(snapshot.ID) || len(snapshot.ManifestSHA256) != 64 || snapshot.Manifest.Bytes < 0 {
		return CollaborationArtifact{}, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationArtifact{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationArtifact{}, err
	}
	if r.Revision != revision || r.State != CollabUncertain {
		return CollaborationArtifact{}, ErrChatConflict
	}
	if err = collaborationStoppedTx(tx, run); err != nil {
		return CollaborationArtifact{}, err
	}
	var step string
	if err = tx.QueryRow("SELECT id FROM collaboration_steps WHERE run_id=? AND state='uncertain'", run).Scan(&step); err != nil {
		return CollaborationArtifact{}, err
	}
	var used int64
	if err = tx.QueryRow("SELECT COALESCE(SUM(size_bytes),0) FROM collaboration_artifacts WHERE run_id=? AND state='ready'", run).Scan(&used); err != nil {
		return CollaborationArtifact{}, err
	}
	if snapshot.Manifest.Bytes > CollaborationArtifactBudget-used {
		return CollaborationArtifact{}, collaboration.ErrInvalid
	}
	a := CollaborationArtifact{ID: snapshot.ID, RunID: run, StepID: step, Kind: "inspection", StorageKey: "snapshots/" + snapshot.ID, SHA256: snapshot.ManifestSHA256, SizeBytes: snapshot.Manifest.Bytes, State: "ready", CreatedAt: usageTimestamp(time.Now())}
	if _, err = tx.Exec("INSERT INTO collaboration_artifacts("+collaborationArtifactCols+") VALUES(?,?,?,?,?,?,?,?,?)", a.ID, a.RunID, a.StepID, a.Kind, a.StorageKey, a.SHA256, a.SizeBytes, a.State, a.CreatedAt); err != nil {
		return a, err
	}
	if err = collaborationEventTx(tx, run, "artifact", a); err != nil {
		return a, err
	}
	return a, tx.Commit()
}
