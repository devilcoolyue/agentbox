package server

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"agentbox/internal/archivex"
	"agentbox/internal/collaboration"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

func (s *Server) verifyCollaboration(ctx context.Context, u store.User, r store.CollaborationRun) (resultErr error) {
	v, fresh, err := s.store.BeginCollaborationVerification(u, r.ID, r.Revision)
	if err != nil {
		return err
	}
	started := time.Now()
	finalized := false
	// Cover failures before the helper starts too (missing/tampered snapshots,
	// failed mounts, cancellation). A claimed verification must not stay running.
	defer func() {
		if recover() != nil {
			resultErr = errors.Join(errors.New("verification panicked"), dockerx.ErrProjectCleanup)
		}
		if fresh && v.State == "running" && !finalized {
			state := "failed"
			if errors.Is(resultErr, dockerx.ErrProjectCleanup) {
				state = "uncertain"
			}
			resultErr = errors.Join(resultErr, s.store.FinishCollaborationVerification(u, v, state, "Verification did not complete successfully.", time.Since(started)))
		}
	}()
	artifact, err := s.store.CollaborationArtifact(u, r.ID, v.SnapshotID)
	if err != nil {
		return err
	}
	root, err := s.openDataDir(s.collaborationDir(u, r.ID))
	if err != nil {
		return err
	}
	defer root.Close()
	snapshots, err := root.Sub("snapshots")
	if err != nil {
		return err
	}
	defer snapshots.Close()
	snapshot, err := collaboration.VerifySnapshot(ctx, snapshots, artifact.ID, artifact.SHA256, collaborationSnapshotLimits)
	if err != nil {
		return err
	}
	if v.State == "running" && fresh {
		output, verifyErr := func() (output string, resultErr error) {
			name := "staging/verify-" + v.HelperID
			if e := root.Mkdir(name, 0700); e != nil {
				return "", e
			}
			defer func() {
				if !errors.Is(resultErr, dockerx.ErrProjectCleanup) {
					resultErr = errors.Join(resultErr, root.RemoveAll(name))
				}
			}()
			dest, e := root.Sub(name)
			if e != nil {
				return "", e
			}
			defer dest.Close()
			tree, e := snapshots.Sub(artifact.ID + "/tree")
			if e != nil {
				return "", e
			}
			defer tree.Close()
			if e = collaboration.MaterializeSnapshot(ctx, tree, dest, snapshot); e != nil {
				return "", e
			}
			if e = archivex.ChownRoot(dest, dockerx.AgentUID, dockerx.AgentGID); e != nil {
				return "", e
			}
			image, e := s.store.CollaborationImage(u, r.ID)
			if e != nil {
				return "", e
			}
			testCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			var commandResult dockerx.CommandResult
			v.Argv = dockerx.CollaborationVerificationArgv(v.Command)
			e = s.workspaces().WithCollaborationHelper(testCtx, u.Name, func() error {
				var runErr error
				commandResult, runErr = s.dock.VerifyCollaborationCommand(testCtx, image, filepath.Join(s.collaborationDir(u, r.ID), name), v.Command, v.HelperID)
				return runErr
			})
			output = commandResult.Output
			v.ExitCode, v.OutputTruncated = commandResult.ExitCode, commandResult.Truncated
			if e != nil {
				return output, e
			}
			checkName := "staging/verify-check-" + store.NewCollaborationID()
			if e = root.Mkdir(checkName, 0700); e != nil {
				return output, e
			}
			defer func() { resultErr = errors.Join(resultErr, root.RemoveAll(checkName)) }()
			checkRoot, e := root.Sub(checkName)
			if e != nil {
				return output, e
			}
			defer checkRoot.Close()
			after, e := collaboration.PublishSnapshot(ctx, dest, checkRoot, store.NewCollaborationID(), collaborationSnapshotLimits)
			if e != nil {
				return output, e
			}
			// Track original code paths, allowing additional build outputs only.
			original := map[string]collaboration.SnapshotEntry{}
			for _, file := range snapshot.Manifest.Entries {
				original[file.Path] = file
			}
			for _, file := range after.Manifest.Entries {
				if old, ok := original[file.Path]; ok {
					if old != file {
						return output, errors.New("verification changed source")
					}
					delete(original, file.Path)
				} else if !collaboration.VerificationOutputPath(file.Path) {
					return output, errors.New("verification created files outside build output directories")
				}
			}
			if len(original) != 0 {
				return output, errors.New("verification removed source")
			}
			return output, nil
		}()
		state := "completed"
		if verifyErr != nil {
			state = "failed"
			if errors.Is(verifyErr, dockerx.ErrProjectCleanup) {
				state = "uncertain"
			}
			output += "\nVerification did not complete successfully."
		}
		if len(output) > 120<<10 {
			v.OutputTruncated = true
			output = output[:120<<10] + "\n[output truncated]"
		}
		if err = s.store.FinishCollaborationVerification(u, v, state, output, time.Since(started)); err != nil {
			return errors.Join(verifyErr, err)
		}
		finalized = true
		if state == "uncertain" {
			return verifyErr
		}
	} else if v.State != "completed" && v.State != "failed" && v.State != "not_requested" {
		return collaboration.ErrState
	}
	current, err := s.store.Collaboration(u, r.ID)
	if err != nil {
		return err
	}
	cfg, err := s.store.CollaborationConfig(r.ID, current.ConfigVersion)
	if err != nil {
		return err
	}
	var evidence *collaboration.ReviewEvidence
	if cfg.Workflow != nil && !cfg.Workflow.IsDirect() {
		evidence, err = s.collaborationReviewEvidence(ctx, u, current, v.SnapshotID)
		if err != nil {
			return err
		}
	}
	if evidence != nil {
		_, err = s.store.QueueCollaborationReview(u, r.ID, current.Revision, evidence)
	} else {
		_, err = s.store.QueueCollaborationReview(u, r.ID, current.Revision)
	}
	return err
}

// Explicit controls and cancellation may reconcile unknown helper outcomes.
// This never executes a verification command or turns unknown into success.
func (s *Server) quiesceCollaboration(ctx context.Context, u store.User, run string, revision int64) error {
	pending, err := s.store.UncertainCollaborationVerifications(u, run)
	if err != nil {
		return err
	}
	for _, v := range pending {
		if err = s.dock.RemoveCollaborationVerification(ctx, v.HelperID); err != nil {
			return err
		}
		root, e := s.openDataDir(s.collaborationDir(u, run))
		if e != nil {
			return e
		}
		e = root.RemoveAll("staging/verify-" + v.HelperID)
		root.Close()
		if e != nil {
			return e
		}
		if err = s.store.ConfirmCollaborationVerificationStopped(u, v); err != nil {
			return err
		}
	}
	return s.workspaces().QuiesceCollaboration(ctx, u, run, revision)
}
