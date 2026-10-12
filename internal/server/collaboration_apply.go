package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/gitx"
	"agentbox/internal/store"
)

func (s *Server) handleCollaborationApplyGet(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	a, err := s.store.CollaborationApplication(reqUser(r), r.PathValue("run"))
	if id := r.URL.Query().Get("action"); id != "" {
		a, err = s.store.CollaborationApplicationByID(reqUser(r), r.PathValue("run"), id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 200, map[string]any{"version": 1, "application": nil})
		return
	}
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "application": a})
}
func (s *Server) handleCollaborationApply(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	var input struct {
		Scope    string `json:"scope"`
		Revision int64  `json:"expected_revision"`
		Snapshot string `json:"snapshot_id"`
	}
	if err = decodeCollaboration(w, r, &input); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if input.Scope != s.collaborationScope(u) {
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	id := r.PathValue("action")
	if prior, e := s.store.CollaborationApplicationByID(u, run.ID, id); e == nil {
		if prior.ActionID != id || prior.SnapshotID != input.Snapshot {
			writeCollaborationError(w, r, store.ErrChatConflict)
			return
		}
		writeJSON(w, 200, map[string]any{"version": 1, "application": prior, "replayed": true})
		return
	} else if !errors.Is(e, sql.ErrNoRows) {
		writeCollaborationError(w, r, e)
		return
	}
	if !s.collaborationApplyMu.TryLock() {
		writeErr(w, 409, "空间仍有文件操作，请稍后再应用。")
		return
	}
	defer s.collaborationApplyMu.Unlock()
	room := s.chat.room(run.SourceSessionID)
	if err = room.begin(); err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	defer room.end()
	reservation := r.Clone(r.Context())
	reservation.ContentLength = 320 << 20
	release, ok := s.reserveStorageWrite(reservation)
	if !ok {
		writeErr(w, 507, "磁盘空间不足，无法准备结果副本。")
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	err = s.workspaces().WithSession(ctx, run.SourceSessionID, func(source store.Session) error {
		if source.User != u.Name {
			return sql.ErrNoRows
		}
		if _, err := s.sessionAccount(source); err != nil {
			return err
		}
		if source.ContainerID != "" {
			running, err := s.dock.Running(ctx, source.ContainerID)
			if err != nil {
				return err
			}
			if running {
				return errors.New("请先停止当前工作空间，再应用协作结果")
			}
		}
		if _, err := s.store.PendingChatRequest(source.ID); err == nil {
			return store.ErrChatPending
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if run.State != store.CollabCompleted || run.Request == nil || run.Revision != input.Revision {
			return store.ErrChatConflict
		}
		artifact, err := s.store.CollaborationLatestArtifact(u, run.ID)
		if err != nil {
			return err
		}
		if artifact.ID != input.Snapshot {
			return store.ErrChatConflict
		}
		baselineArtifact, err := s.store.CollaborationBaselineArtifact(u, run.ID)
		if err != nil {
			return err
		}
		snapshots, err := s.openDataDir(filepath.Join(s.collaborationDir(u, run.ID), "snapshots"))
		if err != nil {
			return err
		}
		defer snapshots.Close()
		before, err := collaboration.VerifySnapshot(ctx, snapshots, baselineArtifact.ID, baselineArtifact.SHA256, collaborationSnapshotLimits)
		if err != nil {
			return err
		}
		after, err := collaboration.VerifySnapshot(ctx, snapshots, artifact.ID, artifact.SHA256, collaborationSnapshotLimits)
		if err != nil {
			return err
		}
		tree, err := snapshots.Sub(after.ID + "/tree")
		if err != nil {
			return err
		}
		defer tree.Close()
		image, err := s.store.CollaborationImage(u, run.ID)
		if err != nil {
			return err
		}
		err = s.workspaces().WithCollaborationHelper(ctx, u.Name, func() error {
			return s.dock.WithProjectReader(ctx, image, s.workspaceDir(source), func(cid string) error {
				git := gitx.New(s.dock, func(context.Context, string) (string, func(), error) { return cid, func() {}, nil })
				head, e := checkCollaborationSource(func(args ...string) (string, error) { return git.Run(ctx, source.ID, run.Request.Repository, args...) })
				if e != nil {
					return e
				}
				if head != run.Request.Baseline {
					return errors.New("源项目已变化，请重新创建协作任务")
				}
				return nil
			})
		})
		if err != nil {
			return err
		}
		application, fresh, err := s.store.BeginCollaborationApplication(u, run.ID, id, after.ID, input.Revision)
		if err != nil {
			return err
		}
		if !fresh {
			return store.ErrChatConflict
		}
		state := application.State
		defer func() {
			if state == "staging" {
				_ = s.store.SetCollaborationApplication(u, run.ID, id, "staging", "failed")
			}
		}()
		base, err := s.openDataDir(filepath.Dir(s.workspaceDir(source)))
		if err != nil {
			return err
		}
		defer base.Close()
		stage := "collaboration-apply-" + id
		backup := "collaboration-before-" + id
		if err = base.Mkdir(stage, 0700); err != nil {
			return err
		}
		defer func() {
			if state == "staging" {
				_ = base.RemoveAll(stage)
			}
		}()
		staged, err := base.Sub(stage)
		if err != nil {
			return err
		}
		defer staged.Close()
		old, err := base.Sub("workspace")
		if err != nil {
			return err
		}
		defer old.Close()
		// Bounded complete copy retains .git and ignored assets. Failure leaves the
		// original workspace untouched and never weakens Linux chown requirements.
		if err = collaboration.CloneWorkspace(ctx, old, staged, 512<<20, 50000); err != nil {
			_ = base.RemoveAll(stage)
			return err
		}
		if err = staged.SetDirMetadata(".", 0755, 1000, 1000, time.Time{}); err != nil {
			return err
		}
		project, err := staged.Sub(run.Request.Repository)
		if err != nil {
			return err
		}
		defer project.Close()
		if err = collaboration.ApplySnapshot(ctx, project, tree, before, after); err != nil {
			_ = base.RemoveAll(stage)
			return err
		}
		if err = s.store.SetCollaborationApplication(u, run.ID, id, "staging", "switching"); err != nil {
			return err
		}
		state = "switching"
		if err = base.RenameTo("workspace", base, backup, false); err != nil {
			return err
		}
		if err = base.Sync(); err != nil {
			return err
		}
		if err = base.RenameTo(stage, base, "workspace", false); err != nil {
			return err
		}
		if err = base.Sync(); err != nil {
			return err
		}
		// Completed receipt follows the directory fsync. The old tree is retained
		// as a recovery copy; it is never part of the active workspace mount.
		if err = s.store.SetCollaborationApplication(u, run.ID, id, "switching", "completed"); err != nil {
			return err
		}
		state = "completed"
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, "结果尚未应用：请确认空间已停止、源项目无新改动，并核对应用状态。")
		return
	}
	a, err := s.store.CollaborationApplication(u, run.ID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "application": a})
}

// Recovery is explicit and read/rename only: never rerun a model or replay an
// application. Return to the untouched original, or acknowledge no switch.
func (s *Server) handleCollaborationApplyRecover(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	var in struct {
		Scope    string `json:"scope"`
		ActionID string `json:"action_id"`
		Snapshot string `json:"snapshot_id,omitempty"`
	}
	if err = decodeCollaboration(w, r, &in); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Scope != s.collaborationScope(u) {
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	a, err := s.store.CollaborationApplicationByID(u, run.ID, in.ActionID)
	if errors.Is(err, sql.ErrNoRows) {
		if in.Snapshot == "" {
			artifact, e := s.store.CollaborationLatestArtifact(u, run.ID)
			if e != nil {
				writeCollaborationError(w, r, e)
				return
			}
			in.Snapshot = artifact.ID
		}
		if !s.collaborationApplyMu.TryLock() {
			writeErr(w, 409, "项目操作尚未结束，请稍后重试。")
			return
		}
		defer s.collaborationApplyMu.Unlock()
		if err = s.store.AbandonCollaborationApplication(u, run.ID, in.ActionID, in.Snapshot); err != nil {
			writeCollaborationError(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"version": 1, "state": "failed"})
		return
	}
	if err != nil || a.ActionID != in.ActionID {
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	if a.State == "completed" || a.State == "failed" {
		writeJSON(w, 200, map[string]any{"version": 1, "application": a})
		return
	}
	if !s.collaborationApplyMu.TryLock() {
		writeErr(w, 409, "项目操作尚未结束，请稍后重试。")
		return
	}
	defer s.collaborationApplyMu.Unlock()
	a, err = s.store.CollaborationApplicationByID(u, run.ID, in.ActionID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if a.State == "completed" || a.State == "failed" {
		writeJSON(w, 200, map[string]any{"version": 1, "application": a})
		return
	}
	err = s.workspaces().WithSession(r.Context(), a.SessionID, func(source store.Session) error {
		if source.User != u.Name {
			return sql.ErrNoRows
		}
		if source.ContainerID != "" {
			running, e := s.dock.Running(r.Context(), source.ContainerID)
			if e != nil {
				return e
			}
			if running {
				return store.ErrChatConflict
			}
		}
		base, e := s.openDataDir(filepath.Dir(s.workspaceDir(source)))
		if e != nil {
			return e
		}
		defer base.Close()
		backup := "collaboration-before-" + a.ActionID
		if _, e = base.Lstat(backup); e == nil {
			if _, e = base.Lstat("workspace"); e == nil {
				if e = base.RenameTo("workspace", base, "collaboration-review-"+a.ActionID, false); e != nil {
					return e
				}
			} else if !os.IsNotExist(e) {
				return e
			}
			if e = base.RenameTo(backup, base, "workspace", false); e != nil {
				return e
			}
		} else if !os.IsNotExist(e) {
			return e
		} else {
			if _, e = base.Lstat("workspace"); e != nil {
				return e
			}
		}
		if e = base.Sync(); e != nil {
			return e
		}
		return s.store.SetCollaborationApplication(u, run.ID, a.ActionID, a.State, "failed")
	})
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	a.State = "failed"
	writeJSON(w, 200, map[string]any{"version": 1, "application": a})
}
