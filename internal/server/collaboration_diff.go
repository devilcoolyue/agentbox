package server

import (
	"agentbox/internal/collaboration"
	"agentbox/internal/safefs"
	"io"
	"net/http"
	"path/filepath"
)

func (s *Server) handleCollaborationDiff(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	before, err := s.store.CollaborationBaselineArtifact(u, run.ID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	after, err := s.store.CollaborationLatestArtifact(u, run.ID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	// Callers can pin the visible version; a newer snapshot is never silently substituted.
	if id := r.URL.Query().Get("snapshot"); id != "" {
		after, err = s.store.CollaborationArtifact(u, run.ID, id)
		if err != nil {
			writeCollaborationError(w, r, err)
			return
		}
	}
	root, err := s.openDataDir(filepath.Join(s.collaborationDir(u, run.ID), "snapshots"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer root.Close()
	a, err := collaboration.VerifySnapshot(r.Context(), root, before.ID, before.SHA256, collaborationSnapshotLimits)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	b, err := collaboration.VerifySnapshot(r.Context(), root, after.ID, after.SHA256, collaborationSnapshotLimits)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	changes := collaboration.SnapshotChanges(a.Manifest, b.Manifest)
	result := map[string]any{"version": 1, "scope": s.collaborationScope(u), "run_id": run.ID, "baseline": before, "snapshot": after, "changes": changes}
	if path := r.URL.Query().Get("path"); path != "" {
		var change *collaboration.FileChange
		for i := range changes {
			if changes[i].Path == path {
				change = &changes[i]
				break
			}
		}
		if change == nil {
			writeCollaborationError(w, r, collaboration.ErrInvalid)
			return
		}
		read := func(tree *safefs.Root, id string, e *collaboration.SnapshotEntry) (string, error) {
			if e == nil || e.Kind == "directory" {
				return "", nil
			}
			if e.Kind == "symlink" {
				return e.Target, nil
			}
			f, err := tree.OpenFile(id + "/tree/" + e.Path)
			if err != nil {
				return "", err
			}
			defer f.Close()
			raw, err := io.ReadAll(io.LimitReader(f, 256<<10))
			return string(raw), err
		}
		oversized := (change.Before != nil && change.Before.Size > 256<<10) || (change.After != nil && change.After.Size > 256<<10)
		result["too_large"] = oversized
		if !oversized {
			old, e := read(root, a.ID, change.Before)
			if e != nil {
				writeCollaborationError(w, r, e)
				return
			}
			next, e := read(root, b.ID, change.After)
			if e != nil {
				writeCollaborationError(w, r, e)
				return
			}
			diff, binary := collaboration.TextDiff(path, old, next)
			result["diff"], result["binary"] = diff, binary
		}
	}
	writeJSON(w, 200, result)
}
