package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"agentbox/internal/filesearch"
	"agentbox/internal/store"
)

// The composer's "@" suggestions read names straight from the workspace on the
// host: they work while the container is stopped, and only names leave the
// server. The CLI runs in /workspace, so the paths are relative to it.
const (
	fileMentionsTTL     = 10 * time.Second
	fileMentionsTimeout = 3 * time.Second
	fileMentionsMax     = 100
	fileMentionsDefault = 40
)

type fileMention struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitempty"`
}

type fileMentionsView struct {
	Files []fileMention `json:"files"`
	// The walk stopped at a limit: deeper files may be missing from results.
	Truncated bool `json:"truncated,omitempty"`
}

func (s *Server) handleFileMentions(w http.ResponseWriter, r *http.Request, sess store.Session) {
	query := r.URL.Query().Get("q")
	if len(query) > 512 {
		writeErr(w, http.StatusBadRequest, "query too long")
		return
	}
	limit := fileMentionsDefault
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > fileMentionsMax {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	root, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	ix, err := s.fileSearch.Get(r.Context(), sess.ID, fileMentionsTTL, func() *filesearch.Index {
		// Not the request's context: the next keystroke aborts this request,
		// and the index it was building is what that keystroke needs.
		ctx, cancel := context.WithTimeout(s.workContext(), fileMentionsTimeout)
		defer cancel()
		return filesearch.Build(ctx, root, filesearch.DefaultLimits)
	})
	if err != nil {
		return // the client went away while another request built the index
	}
	view := fileMentionsView{Files: []fileMention{}, Truncated: ix.Truncated}
	for _, e := range ix.Search(query, limit) {
		view.Files = append(view.Files, fileMention{Path: e.Path, Dir: e.Dir})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}
