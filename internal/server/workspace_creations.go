package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbox/internal/store"
)

type creationWork struct {
	sync.Mutex
	active map[string]bool
}

func (s *Server) beginCreationWork(id string) bool {
	s.creationWork.Lock()
	defer s.creationWork.Unlock()
	if s.creationWork.active == nil {
		s.creationWork.active = map[string]bool{}
	}
	if s.creationWork.active[id] {
		return false
	}
	s.creationWork.active[id] = true
	return true
}
func (s *Server) endCreationWork(id string) {
	s.creationWork.Lock()
	defer s.creationWork.Unlock()
	delete(s.creationWork.active, id)
}
func (s *Server) creationBusy(id string) bool {
	s.creationWork.Lock()
	defer s.creationWork.Unlock()
	return s.creationWork.active[id]
}

func validProjectDirectory(name string) bool {
	return name == strings.TrimSpace(name) && gitText(name, 128, false) && !strings.ContainsAny(name, "/\\") && !strings.HasPrefix(name, ".") && filepath.IsLocal(name)
}
func writeCreationError(w http.ResponseWriter, r *http.Request, err error) {
	code := classifyProblem(err, "internal_error")
	switch {
	case errors.Is(err, store.ErrCreationConflict):
		code = "creation_conflict"
	case errors.Is(err, store.ErrCreationGone):
		code = "creation_gone"
	case errors.Is(err, store.ErrImportPending):
		code = "import_pending"
	case errors.Is(err, sql.ErrNoRows):
		code = "session_not_found"
	}
	writeProblem(w, r, "workspace.create", code)
}

type creationView struct {
	store.WorkspaceCreation
	Session            sessionView             `json:"session"`
	WorkspaceExists    bool                    `json:"workspace_exists"`
	Busy               bool                    `json:"busy"`
	Imports            []store.WorkspaceImport `json:"imports"`
	ContainerResources creationResources       `json:"container_resources"`
}

func (s *Server) creationView(u store.User, c store.WorkspaceCreation) (creationView, error) {
	imports, err := s.store.WorkspaceImports(u, c.RequestID)
	if err != nil {
		return creationView{}, err
	}
	busy := s.creationBusy(c.Session.ID)
	for i := range imports {
		if imports[i].State == "running" && !busy {
			imports[i].State = "uncertain"
		}
	}
	existing, exists := s.store.Get(c.Session.ID)
	exists = exists && existing.User == u.Name
	if c.State == "ready" || c.State == "complete" {
		current, ok := s.store.Get(c.Session.ID)
		if !ok || current.User != u.Name {
			c.State = "abandoned"
		} else {
			c.Session = current
		}
	}
	return creationView{WorkspaceCreation: c, Session: s.view(c.Session), WorkspaceExists: exists, Busy: busy, Imports: imports, ContainerResources: s.creationResources()}, nil
}
func (s *Server) handleCreationList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.PendingWorkspaceCreations(reqUser(r))
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	views := []creationView{}
	for _, row := range rows {
		view, err := s.creationView(reqUser(r), row)
		if err != nil {
			writeCreationError(w, r, err)
			return
		}
		views = append(views, view)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"version": 1, "actor_key": importFingerprint([]string{reqUser(r).Name, reqUser(r).CreatedAt.UTC().Format(time.RFC3339Nano)}), "creations": views})
}
func (s *Server) handleCreationGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.WorkspaceCreation(reqUser(r), r.PathValue("request"))
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	v, err := s.creationView(reqUser(r), c)
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, v)
}
func (s *Server) handleCreationAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Action    string `json:"action"`
		AttemptID string `json:"attempt_id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		writeProblem(w, r, "workspace.create", "invalid_request")
		return
	}
	u := reqUser(r)
	id := r.PathValue("request")
	c, err := s.store.WorkspaceCreation(u, id)
	if errors.Is(err, sql.ErrNoRows) && input.Action == "abandon" && operationIDPattern.MatchString(id) {
		err = s.store.AbandonUnknownWorkspaceCreation(u, id, newOperationID())
		if err == nil {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		if errors.Is(err, store.ErrCreationConflict) {
			c, err = s.store.WorkspaceCreation(u, id)
		}
	}
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	if !s.beginCreationWork(c.Session.ID) {
		writeCreationError(w, r, store.ErrImportPending)
		return
	}
	defer s.endCreationWork(c.Session.ID)
	switch input.Action {
	case "finish":
		err = s.store.FinishWorkspaceCreation(u, id, false)
	case "abandon":
		err = s.store.FinishWorkspaceCreation(u, id, true)
	case "review":
		if !operationIDPattern.MatchString(input.AttemptID) {
			writeProblem(w, r, "workspace.create", "invalid_request")
			return
		}
		err = s.store.ReviewWorkspaceImport(u, id, input.AttemptID)
	default:
		writeProblem(w, r, "workspace.create", "invalid_request")
		return
	}
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) importCreation(w http.ResponseWriter, r *http.Request) (store.WorkspaceCreation, store.Session, bool) {
	c, err := s.store.WorkspaceCreation(reqUser(r), r.PathValue("request"))
	if err != nil {
		writeCreationError(w, r, err)
		return c, store.Session{}, false
	}
	if c.State != "ready" {
		writeCreationError(w, r, store.ErrCreationGone)
		return c, store.Session{}, false
	}
	sess, ok := s.store.Get(c.Session.ID)
	if !ok || sess.User != reqUser(r).Name {
		writeCreationError(w, r, store.ErrCreationGone)
		return c, store.Session{}, false
	}
	return c, sess, true
}

// Existing clone handlers only write small JSON responses. Capture their result
// to persist an import receipt before acknowledging the outer operation.
type importResponse struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (c *importResponse) Header() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}
func (c *importResponse) WriteHeader(n int) {
	if c.status == 0 {
		c.status = n
	}
}
func (c *importResponse) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if c.body.Len()+len(p) > 64<<10 {
		c.overflow = true
	} else {
		c.body.Write(p)
	}
	return len(p), nil
}
