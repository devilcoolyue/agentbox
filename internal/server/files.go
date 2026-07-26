package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentbox/internal/archivex"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// filesRoot resolves which directory a files request operates on: the session
// workspace (default) or the per-user shared directory (?scope=shared).
func (s *Server) filesRoot(r *http.Request, sess store.Session) (string, error) {
	return s.filesRootForScope(r.URL.Query().Get("scope"), sess)
}

func (s *Server) filesRootForScope(scope string, sess store.Session) (string, error) {
	switch scope {
	case "", "workspace":
		return s.workspaceDir(sess), nil
	case "shared":
		return s.ensureSharedDir(sess.User)
	default:
		return "", fmt.Errorf("invalid scope")
	}
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

var (
	errFileOpInvalid  = errors.New("invalid file operation")
	errFileOpConflict = errors.New("destination already exists")
)

// resolveFileEntry resolves a non-root path and rejects symlinks in every
// component. File operations must not be able to follow a workspace symlink
// into an arbitrary host path.
func resolveFileEntry(root, raw string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(raw))
	if rel == "." || rel == "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: invalid path", errFileOpInvalid)
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: symlinks are not supported", errFileOpInvalid)
		}
	}
	return cur, nil
}

// resolveFileDir is the directory counterpart of resolveFileEntry. The root
// itself is a valid destination, while every nested component must already
// exist and be a real directory.
func resolveFileDir(root, raw string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(raw))
	if rel == "" {
		rel = "."
	}
	if !filepath.IsLocal(rel) && rel != "." {
		return "", fmt.Errorf("%w: invalid destination", errFileOpInvalid)
	}
	cur := root
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			info, err := os.Lstat(cur)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("%w: destination is not a directory", errFileOpInvalid)
			}
		}
	}
	return cur, nil
}

func removeFileEntry(root, rel string) error {
	p, err := resolveFileEntry(root, rel)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

func moveFileEntry(sourceRoot, sourceRel, destinationRoot, destinationDirRel string) (string, error) {
	source, err := resolveFileEntry(sourceRoot, sourceRel)
	if err != nil {
		return "", err
	}
	destinationDir, err := resolveFileDir(destinationRoot, destinationDirRel)
	if err != nil {
		return "", err
	}

	if info, err := os.Lstat(source); err != nil {
		return "", err
	} else if info.IsDir() {
		rel, relErr := filepath.Rel(source, destinationDir)
		if relErr == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			return "", fmt.Errorf("%w: cannot move a directory into itself", errFileOpInvalid)
		}
	}

	destination := filepath.Join(destinationDir, filepath.Base(source))
	if filepath.Clean(source) == filepath.Clean(destination) {
		return "", fmt.Errorf("%w: source is already in this directory", errFileOpInvalid)
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", errFileOpConflict
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := renameNoReplace(source, destination); err != nil {
		if os.IsExist(err) {
			return "", errFileOpConflict
		}
		return "", err
	}
	rel, err := filepath.Rel(destinationRoot, destination)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func writeFileOpErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errFileOpInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errFileOpConflict):
		writeErr(w, http.StatusConflict, "目标目录中已存在同名文件或目录")
	case os.IsNotExist(err):
		writeErr(w, http.StatusNotFound, "文件或目录不存在")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request, sess store.Session) {
	root, err := s.filesRoot(r, sess)
	if err == nil {
		err = removeFileEntry(root, r.URL.Query().Get("path"))
	}
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

type moveFileRequest struct {
	SourceScope      string `json:"source_scope"`
	SourcePath       string `json:"source_path"`
	DestinationScope string `json:"destination_scope"`
	DestinationDir   string `json:"destination_dir"`
}

func (s *Server) handleFileMove(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req moveFileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sourceRoot, err := s.filesRootForScope(req.SourceScope, sess)
	if err != nil {
		writeFileOpErr(w, fmt.Errorf("%w: invalid source scope", errFileOpInvalid))
		return
	}
	destinationRoot, err := s.filesRootForScope(req.DestinationScope, sess)
	if err != nil {
		writeFileOpErr(w, fmt.Errorf("%w: invalid destination scope", errFileOpInvalid))
		return
	}
	destinationPath, err := moveFileEntry(sourceRoot, req.SourcePath, destinationRoot, req.DestinationDir)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"scope": req.DestinationScope,
		"path":  destinationPath,
	})
}

// validFileName accepts a single non-empty path component (no separators, not
// "."/".."), used by mkdir and rename where only the leaf name is user-supplied.
func validFileName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return name, true
}

type mkdirRequest struct {
	Scope string `json:"scope"`
	Dir   string `json:"dir"`  // 父目录（相对根，空=根）
	Name  string `json:"name"` // 新目录名（单段）
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req mkdirRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	root, err := s.filesRootForScope(req.Scope, sess)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	name, ok := validFileName(req.Name)
	if !ok {
		writeFileOpErr(w, fmt.Errorf("%w: invalid name", errFileOpInvalid))
		return
	}
	parent, err := resolveFileDir(root, req.Dir)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	target := filepath.Join(parent, name)
	if err := os.Mkdir(target, 0o755); err != nil {
		if os.IsExist(err) {
			writeErr(w, http.StatusConflict, "目标目录中已存在同名文件或目录")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Chown(target, dockerx.AgentUID, dockerx.AgentGID)
	rel, _ := filepath.Rel(root, target)
	writeJSON(w, http.StatusOK, map[string]string{"path": filepath.ToSlash(rel)})
}

type renameRequest struct {
	Scope string `json:"scope"`
	Path  string `json:"path"` // 现有条目（相对根）
	Name  string `json:"name"` // 新名（单段，同目录内）
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req renameRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	root, err := s.filesRootForScope(req.Scope, sess)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	name, ok := validFileName(req.Name)
	if !ok {
		writeFileOpErr(w, fmt.Errorf("%w: invalid name", errFileOpInvalid))
		return
	}
	source, err := resolveFileEntry(root, req.Path)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	dest := filepath.Join(filepath.Dir(source), name)
	if filepath.Clean(source) == filepath.Clean(dest) {
		rel, _ := filepath.Rel(root, source)
		writeJSON(w, http.StatusOK, map[string]string{"path": filepath.ToSlash(rel)})
		return
	}
	if err := renameNoReplace(source, dest); err != nil {
		if os.IsExist(err) {
			writeErr(w, http.StatusConflict, "目标目录中已存在同名文件或目录")
			return
		}
		writeFileOpErr(w, err)
		return
	}
	rel, _ := filepath.Rel(root, dest)
	writeJSON(w, http.StatusOK, map[string]string{"path": filepath.ToSlash(rel)})
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
