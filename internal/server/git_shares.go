package server

import (
	"database/sql"
	"errors"
	"net/http"

	"agentbox/internal/store"
)

func (s *Server) handleGitShares(w http.ResponseWriter, r *http.Request) {
	user, id := reqUser(r).Name, r.PathValue("connection")
	if r.Method == http.MethodPut {
		var input struct {
			Revision int64            `json:"revision"`
			Users    []store.GitShare `json:"users"`
		}
		if !decodeGitJSON(w, r, &input) {
			return
		}
		if err := s.store.SetGitShares(user, id, input.Revision, input.Users); err != nil {
			if errors.Is(err, store.ErrGitConflict) || errors.Is(err, sql.ErrNoRows) {
				writeGitStoreErr(w, err)
			} else {
				writeErr(w, 400, err.Error())
			}
			return
		}
	}
	users, revision, err := s.store.GitShares(user, id)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"users": users, "revision": revision})
}
