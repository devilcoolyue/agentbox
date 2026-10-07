package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"agentbox/internal/store"
)

var draftAttachmentPath = regexp.MustCompile(`^/shared/\.(images|file)/[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

// Only attachment references are checked; no bytes, directory listings or host
// paths are returned. OpenFile rejects links and non-regular files.
func (s *Server) handleAttachmentValidate(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var input struct {
		Paths []string `json:"paths"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&input) != nil || len(input.Paths) > 64 {
		writeProblem(w, r, "chat.attachments", "invalid_request")
		return
	}
	for _, p := range input.Paths {
		if !draftAttachmentPath.MatchString(p) {
			writeProblem(w, r, "chat.attachments", "invalid_request")
			return
		}
	}
	s.attachmentMu.Lock()
	valid, err := s.validateChatAttachments(sess, input.Paths)
	s.attachmentMu.Unlock()
	if err != nil {
		writeProblem(w, r, "chat.attachments", "internal_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"valid": valid})
}

func (s *Server) validateChatAttachments(sess store.Session, paths []string) ([]bool, error) {
	valid := make([]bool, len(paths))
	if len(paths) == 0 {
		return valid, nil
	}
	root, err := s.openDataDir(filepath.Join(s.cfg.DataDir, "users", sess.User, "shared"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		defer root.Close()
		for i, p := range paths {
			if !draftAttachmentPath.MatchString(p) {
				continue
			}
			f, err := root.OpenFile(strings.TrimPrefix(p, "/shared/"))
			if err == nil {
				info, statErr := f.Stat()
				valid[i] = statErr == nil && info.Mode().IsRegular()
				f.Close()
			}
		}
	}
	return valid, nil
}
