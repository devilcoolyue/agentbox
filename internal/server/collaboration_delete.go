package server

import (
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Server) handleCollaborationDelete(w http.ResponseWriter, r *http.Request) {
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
	if in.Scope == "" || in.Scope != s.collaborationScope(u) {
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	// Serialize cleanup with scheduler ownership, including the small window
	// between a persisted final state and the scheduler's return.
	jobs := &s.collaborationJobs
	jobs.mu.Lock()
	if jobs.running[run.ID] {
		jobs.mu.Unlock()
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	if jobs.running == nil {
		jobs.running = map[string]bool{}
	}
	jobs.running[run.ID] = true
	jobs.mu.Unlock()
	defer func() {
		jobs.mu.Lock()
		again := jobs.again[run.ID]
		delete(jobs.again, run.ID)
		delete(jobs.running, run.ID)
		jobs.mu.Unlock()
		if again {
			s.kickCollaboration(u, run.ID)
		}
	}()
	// Collect any rotated login material before removing a stopped home.
	// Never interrupt an active task merely because a delete request arrived.
	if run.State != store.CollabDeleted && run.State != store.CollabPaused && run.State != store.CollabCompleted && run.State != store.CollabCancelled && run.State != store.CollabFailed {
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	before, e := s.store.CollaborationParticipants(u, run.ID)
	if e != nil {
		writeCollaborationError(w, r, e)
		return
	}
	for _, p := range before {
		if _, exists := s.store.Get(p.SessionID); exists {
			if _, e = s.workspaces().StopCollaborationParticipant(r.Context(), u, run.ID, p.SessionID); e != nil {
				writeCollaborationError(w, r, e)
				return
			}
		}
	}
	if err = s.store.DeleteCollaboration(u, run.ID, in.Scope, in.Revision); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	participants, err := s.store.CollaborationParticipants(u, run.ID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer root.Close()
	for _, p := range participants {
		if _, exists := s.store.Get(p.SessionID); exists {
			if err = s.workspaces().Delete(r.Context(), p.SessionID, true); err != nil {
				writeCollaborationError(w, r, err)
				return
			}
		}
		// A previous attempt may have removed SQLite metadata before a disk error.
		if err = root.RemoveAll(filepath.Join("users", u.Name, "sessions", p.SessionID)); err != nil && !os.IsNotExist(err) {
			writeCollaborationError(w, r, err)
			return
		}
		if err = s.dock.RemoveCollaborationToolCache(u.Name, p.SessionID); err != nil {
			writeCollaborationError(w, r, err)
			return
		}
	}
	if err = root.RemoveAll(filepath.Join("users", u.Name, "collaborations", run.ID)); err != nil && !os.IsNotExist(err) {
		writeCollaborationError(w, r, err)
		return
	}
	if err = s.store.FinishCollaborationDeletion(u, run.ID); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"version": 1, "run_id": run.ID, "deleted": true})
}
