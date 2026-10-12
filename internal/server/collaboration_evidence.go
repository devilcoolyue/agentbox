package server

import (
	"context"
	"path/filepath"

	"agentbox/internal/collaboration"
	"agentbox/internal/store"
)

// The DB chooses both artifacts and the exact output snapshot is pinned by
// the verification receipt. Neither a model-authored path nor the latest
// mutable working tree can select the bytes given to the reviewer.
func (s *Server) collaborationReviewEvidence(ctx context.Context, u store.User, run store.CollaborationRun, snapshotID string) (*collaboration.ReviewEvidence, error) {
	before, err := s.store.CollaborationBaselineArtifact(u, run.ID)
	if err != nil {
		return nil, err
	}
	after, err := s.store.CollaborationArtifact(u, run.ID, snapshotID)
	if err != nil {
		return nil, err
	}
	root, err := s.openDataDir(filepath.Join(s.collaborationDir(u, run.ID), "snapshots"))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	a, err := collaboration.VerifySnapshot(ctx, root, before.ID, before.SHA256, collaborationSnapshotLimits)
	if err != nil {
		return nil, err
	}
	b, err := collaboration.VerifySnapshot(ctx, root, after.ID, after.SHA256, collaborationSnapshotLimits)
	if err != nil {
		return nil, err
	}
	evidence, err := collaboration.BuildReviewEvidence(ctx, root, a, b, run.PlanVersion)
	if err != nil {
		return nil, err
	}
	return &evidence, nil
}
