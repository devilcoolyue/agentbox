package server

import (
	"agentbox/internal/collaboration"
	"agentbox/internal/store"
	"errors"
	"net/http"
)

func (s *Server) handleCollaborationInspection(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	var in struct {
		Scope    string `json:"scope"`
		Revision int64  `json:"expected_revision"`
	}
	if err := decodeCollaboration(w, r, &in); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if in.Scope != s.collaborationScope(u) || in.Revision != run.Revision || run.State != store.CollabUncertain {
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	if err = s.quiesceCollaboration(r.Context(), u, run.ID, run.Revision); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	root, err := s.openDataDir(s.collaborationDir(u, run.ID))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer root.Close()
	work, err := root.Sub("work")
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer work.Close()
	snapshots, err := root.Sub("snapshots")
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer snapshots.Close()
	snapshot, err := collaboration.PublishSnapshot(r.Context(), work, snapshots, store.NewCollaborationID(), collaborationSnapshotLimits)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	artifact, err := s.store.RegisterCollaborationInspection(u, run.ID, run.Revision, snapshot)
	if err != nil {
		err = errors.Join(err, snapshots.RemoveAll(snapshot.ID))
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "run_id": run.ID, "artifact": artifact, "expected_revision": run.Revision})
}
