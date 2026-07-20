package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"agentbox/internal/archivex"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// filesRoot resolves which directory a files request operates on: the session
// workspace (default) or the per-user shared directory (?scope=shared).
func (s *Server) filesRoot(r *http.Request, sess store.Session) (string, error) {
	if r.URL.Query().Get("scope") == "shared" {
		return s.ensureSharedDir(sess.User)
	}
	return s.workspaceDir(sess), nil
}

// handleUpload accepts a multipart "file" field. Archives (.zip/.tar.gz/.tgz/
// .tar) are extracted into the target directory; anything else is stored as a
// single file at its root. "clear=1" empties the target directory first.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, sess store.Session) {
	maxBytes := s.cfg.GetMaxUploadMB() << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "upload too large or malformed: "+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()

	ws, err := s.filesRoot(r, sess)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.FormValue("clear") == "1" {
		if err := clearDir(ws); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if !archivex.IsArchiveName(hdr.Filename) {
		name := filepath.Base(filepath.Clean(hdr.Filename))
		if name == "." || name == ".." || name == "/" {
			writeErr(w, http.StatusBadRequest, "invalid filename")
			return
		}
		dst := filepath.Join(ws, name)
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		_, err = io.Copy(out, file)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = os.Chown(dst, dockerx.AgentUID, dockerx.AgentGID)
		writeJSON(w, http.StatusOK, map[string]any{"files": 1, "mode": "file"})
		return
	}

	// Spool the archive to disk first so zip (which needs random access) works.
	tmp, err := os.CreateTemp(s.sessionDir(sess), "upload-*"+filepath.Ext(hdr.Filename))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp.Close()

	n, err := archivex.Extract(tmp.Name(), hdr.Filename, ws, maxBytes*archivex.ExtractLimitMultiplier)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "extract failed: "+err.Error())
		return
	}
	if err := archivex.ChownTree(ws, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": n, "mode": "archive"})
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, err := s.filesRoot(r, sess)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := sess.Name + "-workspace.zip"
	if r.URL.Query().Get("scope") == "shared" {
		name = "shared.zip"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if err := archivex.ZipDir(w, dir); err != nil {
		// Headers are already sent; all we can do is log via the error path.
		fmt.Fprintf(os.Stderr, "archive %s: %v\n", sess.ID, err)
	}
}

type fileEntry struct {
	Name  string    `json:"name"`
	IsDir bool      `json:"is_dir"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"` // 形如 -rw-r--r-- / drwxr-xr-x
	MTime time.Time `json:"mtime"`
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request, sess store.Session) {
	rel := filepath.FromSlash(r.URL.Query().Get("path"))
	if rel == "" {
		rel = "."
	}
	if !filepath.IsLocal(rel) && rel != "." {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	root, err := s.filesRoot(r, sess)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dir := filepath.Join(root, rel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	out := []fileEntry{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size(), Mode: info.Mode().String(), MTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

// resolveFile validates ?path= and returns the absolute path of a regular
// file inside the request's files root (workspace or shared). Symlinks are
// rejected so the web API can't be led outside the mounted directory.
func (s *Server) resolveFile(r *http.Request, sess store.Session) (string, error) {
	rel := filepath.FromSlash(r.URL.Query().Get("path"))
	if rel == "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("invalid path")
	}
	root, err := s.filesRoot(r, sess)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, rel), nil
}

// handleFileGet streams a single file's raw content; ?dl=1 forces download.
func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request, sess store.Session) {
	p, err := s.resolveFile(r, sess)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Lstat(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !info.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "not a regular file")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
	}
	http.ServeContent(w, r, filepath.Base(p), info.ModTime(), f)
}

// handleFilePut writes the request body as the file's new content (creating
// it if absent) — the save path of the web editor.
func (s *Server) handleFilePut(w http.ResponseWriter, r *http.Request, sess store.Session) {
	p, err := s.resolveFile(r, sess)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "not a regular file")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file too large (max 16MB) or read failed")
		return
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Chown(p, dockerx.AgentUID, dockerx.AgentGID)
	info, _ := os.Stat(p)
	writeJSON(w, http.StatusOK, map[string]any{"size": len(body), "mtime": info.ModTime()})
}
