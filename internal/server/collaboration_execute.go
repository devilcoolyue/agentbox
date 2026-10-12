package server

import (
	"context"
	"errors"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/store"
)

// executeCollaboration performs one run's persisted queue until a user action,
// review/delivery gate or failure is reached. The server lifecycle owns ctx;
// HTTP/WS disconnection must not cancel accepted work. Durable claims remain
// the authority even if two goroutines observe the same pending run.
func (s *Server) executeCollaboration(ctx context.Context, u store.User, id string, emit func(any)) error {
	_, err := s.store.IncorporateCollaborationSupplements(u, id)
	if err != nil {
		return err
	}
	r, err := s.store.Collaboration(u, id)
	if err != nil {
		return err
	}
	if r.State == store.CollabAccepted {
		ready, e := s.resolveCollaborationDecision(ctx, u, r)
		if e != nil || !ready {
			return e
		}
		r, err = s.store.Collaboration(u, id)
		if err != nil {
			return err
		}
		if r.State != store.CollabAccepted {
			return nil
		}
		r, err = s.store.BeginCollaborationPreparation(u, id, r.Revision)
		if err != nil {
			return err
		}
		if r.State != store.CollabPreparing {
			return nil
		}
		prepareCtx, cancelPrepare := context.WithTimeout(ctx, 10*time.Minute)
		prepareErr := func(ctx context.Context) error {
			image, err := s.store.CollaborationImage(u, id)
			if err != nil {
				return err
			}
			if image == "" {
				img, e := s.dock.InspectCLIImage(ctx, s.cfg.GetAgentImage())
				if e != nil {
					return e
				}
				image = img.ID
			}
			if err = s.workspaces().WithCollaborationHelper(ctx, u.Name, func() error { _, e := s.dock.ValidateCollaborationImage(ctx, image); return e }); err != nil {
				return err
			}
			if err = s.store.PinCollaborationImage(u, id, r.Revision, image); err != nil {
				return err
			}
			prepared, e := s.prepareCollaboration(ctx, u, r, image)
			if e == nil {
				r = prepared
			}
			return e
		}(prepareCtx)
		cancelPrepare()
		if prepareErr != nil {
			current, readErr := s.store.Collaboration(u, id)
			if readErr != nil {
				return errors.Join(prepareErr, readErr)
			}
			if current.State == store.CollabPreparing && current.ConfigVersion == r.ConfigVersion {
				_, saveErr := s.store.FailCollaborationPreparation(u, id, current.Revision, "preparation_failed")
				return errors.Join(prepareErr, saveErr)
			}
			return prepareErr
		}
	}
	for ctx.Err() == nil {
		rebuilt, e := s.store.IncorporateCollaborationSupplements(u, id)
		if e != nil {
			return e
		}
		if rebuilt {
			return s.executeCollaboration(ctx, u, id, emit)
		}
		r, err = s.store.Collaboration(u, id)
		if err != nil {
			return err
		}
		switch r.State {
		case store.CollabPaused:
			if r.WaitReason == "verification_required" {
				if err = s.verifyCollaboration(ctx, u, r); err != nil {
					return err
				}
				continue
			}
			if r.WaitReason == "delivery_required" {
				artifact, e := s.store.CollaborationLatestArtifact(u, id)
				if e != nil {
					return e
				}
				root, e := s.openDataDir(s.collaborationDir(u, id) + "/snapshots")
				if e != nil {
					return e
				}
				_, e = collaboration.VerifySnapshot(ctx, root, artifact.ID, artifact.SHA256, collaborationSnapshotLimits)
				root.Close()
				if e != nil {
					return e
				}
				_, err = s.store.DeliverCollaboration(u, id, r.Revision, artifact.ID)
				return err
			}
			return nil
		case store.CollabCancelling:
			if err = s.quiesceCollaboration(ctx, u, id, r.Revision); err != nil {
				return err
			}
			_, err = s.store.FinalizeCollaborationCancellation(u, id, r.Revision)
			return err
		case store.CollabPlanning, store.CollabExecuting, store.CollabReviewing, store.CollabWaitingAnswer:
		default:
			return nil
		}
		work, err := s.store.CollaborationWork(u, id)
		if err != nil {
			return err
		}
		if work.Step.State != "accepted" {
			return store.ErrChatConflict
		}
		step, err := s.store.ClaimCollaborationStep(u, id, work.Step.ID, work.Step.Revision)
		if err != nil {
			return err
		}
		work.Step = step
		if err = s.executeCollaborationStep(ctx, u, work, emit); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Server) executeCollaborationStep(ctx context.Context, u store.User, work store.CollaborationWork, emit func(any)) (resultErr error) {
	// Filesystem/DB failures after model execution must not leave an orphaned
	// runnable-looking step. Never retry the model to reconstruct its result.
	defer func() {
		if recover() != nil {
			resultErr = errors.New("collaboration execution panicked")
		}
		if resultErr == nil {
			return
		}
		step, e := s.store.CollaborationStep(work.Step.ID)
		if e != nil {
			resultErr = errors.Join(resultErr, e)
			return
		}
		if step.State != "starting" && step.State != "running" && step.State != "finalizing" {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, stopErr := s.workspaces().StopCollaborationParticipant(cleanup, u, work.Run.ID, work.Session.ID)
		_, saveErr := s.store.FailCollaborationStep(u, work.Run.ID, step.ID, step.Revision, "execution_failed", stopErr == nil)
		resultErr = errors.Join(resultErr, stopErr, saveErr)
	}()
	image, err := s.store.CollaborationImage(u, work.Run.ID)
	if err != nil {
		return err
	}
	p, err := s.collaborationProfile(u, work, image)
	if err != nil {
		_, saved := s.store.FailCollaborationStep(u, work.Run.ID, work.Step.ID, work.Step.Revision, "execution_failed", true)
		return errors.Join(err, saved)
	}
	turn := s.runCollaborationTurn(ctx, u, work, p, func(value any) {
		if emit != nil {
			emit(map[string]any{"version": 1, "run_id": work.Run.ID, "step_id": work.Step.ID, "role": work.Step.Role, "event": value})
		}
	})
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, stopErr := s.workspaces().StopCollaborationParticipant(cleanup, u, work.Run.ID, work.Session.ID)
	if !turn.Succeeded() || stopErr != nil {
		code := turn.code
		if code == "" {
			code = "execution_failed"
		}
		if stopErr != nil {
			code = "container_stop_failed"
		}
		_, saved := s.store.FailCollaborationStep(u, work.Run.ID, work.Step.ID, turn.work.Step.Revision, code, stopErr == nil)
		return errors.Join(errors.New(code), turn.cause, stopErr, saved)
	}
	final, err := turn.final.Result()
	if err != nil {
		_, saved := s.store.FailCollaborationStep(u, work.Run.ID, work.Step.ID, turn.work.Step.Revision, "execution_failed", true)
		return errors.Join(err, saved)
	}
	step, err := s.store.AdvanceCollaborationStep(u, work.Run.ID, work.Step.ID, turn.work.Step.Revision, "finalizing")
	if err != nil {
		return err
	}
	completion := store.CollaborationCompletion{Final: []byte(final), HistorySynced: true, UsageSettled: true, ProcessStopped: true}
	current, err := s.store.Collaboration(u, work.Run.ID)
	if err != nil {
		return err
	}
	if step.Kind == "handoff_repair" {
		completion.SnapshotID = step.SnapshotID
	}
	if step.Role == "executor" && step.Kind != "handoff_repair" && current.State != store.CollabCancelling {
		taskRoot, err := s.openDataDir(s.collaborationDir(u, work.Run.ID))
		if err != nil {
			return err
		}
		defer taskRoot.Close()
		source, err := taskRoot.Sub("work")
		if err != nil {
			return err
		}
		defer source.Close()
		snapshots, err := taskRoot.Sub("snapshots")
		if err != nil {
			return err
		}
		defer snapshots.Close()
		snapshot, err := collaboration.PublishSnapshot(cleanup, source, snapshots, store.NewCollaborationID(), collaborationSnapshotLimits)
		if err != nil {
			return err
		}
		if _, err = collaboration.VerifySnapshot(cleanup, snapshots, snapshot.ID, snapshot.ManifestSHA256, collaborationSnapshotLimits); err != nil {
			return err
		}
		if _, err = s.store.RegisterCollaborationSnapshot(u, work.Run.ID, current.Revision, step.ID, step.Revision, snapshot); err != nil {
			return errors.Join(err, snapshots.RemoveAll(snapshot.ID))
		}
		completion.SnapshotID = snapshot.ID
	}
	_, err = s.store.CompleteCollaborationStep(u, work.Run.ID, step.ID, step.Revision, completion)
	return err
}
