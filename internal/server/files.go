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
	"agentbox/internal/safefs"
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
		return "", errInvalidScope
	}
}

// handleUpload accepts a multipart "file" field. Archives (.zip/.tar.gz/.tgz/
// .tar) are extracted into the target directory; anything else is stored as a
// single file in it. The target is the scope root, or "?path=" below it (the
// directory the user is browsing). "clear=1" empties only that target first.
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
		writeFileOpErr(w, err)
		return
	}
	area, err := s.openDataDir(ws)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer area.Close()
	// Sub pins each path component, so a directory swapped for a symlink
	// can't redirect the merge (or the clear) outside the scope root.
	root, err := area.Sub(r.URL.Query().Get("path"))
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	clear := r.FormValue("clear") == "1"
	// Stage every upload outside container mounts. A malformed archive cannot
	// leave a partially overwritten live tree even when clear is false.
	stage, err := os.MkdirTemp(s.cfg.DataDir, ".stage-upload-*")
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer os.RemoveAll(stage)
	content := filepath.Join(stage, "content")
	if err := os.Mkdir(content, 0o700); err != nil {
		writeFileOpErr(w, err)
		return
	}
	staged, err := safefs.Open(content)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer staged.Close()
	count, mode := 1, "file"
	if !archivex.IsArchiveName(hdr.Filename) {
		name := filepath.Base(filepath.Clean(hdr.Filename))
		if name == "." || name == ".." || name == "/" {
			writeFileOpErr(w, errFileOpInvalid)
			return
		}
		_, err = staged.WriteAtomic(name, file, safefs.WriteOptions{Mode: 0o644, MaxBytes: maxBytes, Limit: true})
	} else {
		mode = "archive"
		archive := filepath.Join(stage, "archive")
		var f *os.File
		f, err = os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = io.Copy(f, file)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err == nil {
			count, err = archivex.ExtractRoot(archive, hdr.Filename, staged, maxBytes*archivex.ExtractLimitMultiplier)
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Linux ownership errors are significant. Local developers cannot chown;
	// match the editor's best-effort behavior when not running as root.
	if err = archivex.ChownRoot(staged, dockerx.AgentUID, dockerx.AgentGID); err != nil && os.Geteuid() == 0 {
		writeFileOpErr(w, err)
		return
	}
	if clear {
		if err := root.Clear(); err != nil {
			writeFileOpErr(w, err)
			return
		}
	}
	if err := mergeUpload(staged, root); err != nil {
		writeFileOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": count, "mode": mode})
}

func mergeUpload(src, dst *safefs.Root) error {
	entries, err := src.ReadDir(".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		info, err := dst.Lstat(name)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if info != nil && info.Mode()&os.ModeSymlink != 0 {
			return errFileOpInvalid
		}
		if !e.IsDir() || info == nil {
			if err := src.RenameTo(name, dst, name, true); err != nil {
				return err
			}
			continue
		}
		a, err := src.Sub(name)
		if err != nil {
			return err
		}
		b, err := dst.Sub(name)
		if err != nil {
			a.Close()
			return err
		}
		err = mergeUpload(a, b)
		a.Close()
		b.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, err := s.filesRoot(r, sess)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	root, err := s.openDataDir(dir)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	name := sess.Name + "-workspace.zip"
	if r.URL.Query().Get("scope") == "shared" {
		name = "shared.zip"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if err := archivex.ZipRoot(w, root); err != nil {
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
	root, err := s.filesRoot(r, sess)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	dir, err := s.openDataDir(root)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer dir.Close()
	entries, err := dir.ReadDir(r.URL.Query().Get("path"))
	if err != nil {
		writeFileOpErr(w, err)
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
	errFileOpInvalid  = safefs.ErrInvalid
	errFileOpConflict = errors.New("destination already exists")
	errInvalidScope   = errors.New("invalid scope")
)

func removeFileEntry(root, rel string) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || !filepath.IsLocal(rel) {
		return errFileOpInvalid
	}
	dir, err := openFileArea(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Lstat(rel)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errFileOpInvalid
	}
	return dir.RemoveAll(rel)
}

func moveFileEntry(sourceRoot, sourceRel, destinationRoot, destinationDirRel string) (string, error) {
	sourceRel = filepath.Clean(filepath.FromSlash(sourceRel))
	destinationDirRel = filepath.Clean(filepath.FromSlash(destinationDirRel))
	if sourceRel == "." || !filepath.IsLocal(sourceRel) || !filepath.IsLocal(destinationDirRel) {
		return "", errFileOpInvalid
	}
	src, err := openFileArea(sourceRoot)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := openFileArea(destinationRoot)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	info, err := src.Lstat(sourceRel)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errFileOpInvalid
	}
	target := filepath.Join(destinationDirRel, filepath.Base(sourceRel))
	source := filepath.Join(sourceRoot, sourceRel)
	destination := filepath.Join(destinationRoot, target)
	if source == destination {
		return "", errFileOpInvalid
	}
	if info.IsDir() {
		rel, err := filepath.Rel(source, filepath.Join(destinationRoot, destinationDirRel))
		if err == nil && filepath.IsLocal(rel) {
			return "", errFileOpInvalid
		}
	}
	if err := src.RenameTo(sourceRel, dst, target, false); err != nil {
		if os.IsExist(err) {
			return "", errFileOpConflict
		}
		return "", err
	}
	return filepath.ToSlash(target), nil
}

func writeFileOpErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidScope):
		writeErr(w, http.StatusBadRequest, "invalid scope")
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
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
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
	dir, err := s.openDataDir(root)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer dir.Close()
	parent, err := dir.Sub(req.Dir)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer parent.Close()
	if err := parent.Mkdir(name, 0o755); err != nil {
		if os.IsExist(err) {
			err = errFileOpConflict
		}
		writeFileOpErr(w, err)
		return
	}
	_ = parent.Chown(name, dockerx.AgentUID, dockerx.AgentGID)
	writeJSON(w, http.StatusOK, map[string]string{"path": filepath.ToSlash(filepath.Join(req.Dir, name))})
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
	rel := filepath.Clean(filepath.FromSlash(req.Path))
	if rel == "." || !filepath.IsLocal(rel) {
		writeFileOpErr(w, errFileOpInvalid)
		return
	}
	dir, err := s.openDataDir(root)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer dir.Close()
	info, err := dir.Lstat(rel)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		writeFileOpErr(w, errFileOpInvalid)
		return
	}
	target := filepath.Join(filepath.Dir(rel), name)
	if rel != target {
		if err := dir.RenameTo(rel, dir, target, false); err != nil {
			if os.IsExist(err) {
				err = errFileOpConflict
			}
			writeFileOpErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": filepath.ToSlash(target)})
}

// handleFileGet streams a single file's raw content; ?dl=1 forces download.
func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request, sess store.Session) {
	root, err := s.filesRoot(r, sess)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	dir, err := s.openDataDir(root)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer dir.Close()
	name := r.URL.Query().Get("path")
	f, err := dir.OpenFile(name)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(name)))
		http.ServeContent(w, r, filepath.Base(name), info.ModTime(), f)
		return
	}
	serveRawInline(w, r, f, info)
}

// handleFilePut replaces a file atomically through a pinned directory handle.
func (s *Server) handleFilePut(w http.ResponseWriter, r *http.Request, sess store.Session) {
	root, err := s.filesRoot(r, sess)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	dir, err := s.openDataDir(root)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer dir.Close()
	name := r.URL.Query().Get("path")
	mode := os.FileMode(0o644)
	if old, err := dir.Lstat(name); err == nil {
		if !old.Mode().IsRegular() {
			writeFileOpErr(w, errFileOpInvalid)
			return
		}
		mode = old.Mode().Perm()
	} else if !os.IsNotExist(err) {
		writeFileOpErr(w, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file too large (max 16MB) or read failed")
		return
	}
	info, err := dir.WriteFile(name, body, safefs.WriteOptions{Mode: mode, Chown: true, UID: dockerx.AgentUID, GID: dockerx.AgentGID, BestEffortChown: true})
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"size": len(body), "mtime": info.ModTime()})
}

// Only the configured data directory is trusted. Descendants may contain
// container-controlled components and must be opened through Sub.
func (s *Server) openDataDir(dir string) (*safefs.Root, error) {
	rel, err := filepath.Rel(s.cfg.DataDir, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, errFileOpInvalid
	}
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Sub(rel)
}

// Files areas themselves are mount points; pin them through their service-owned
// parent so replacing the mount-point name with a symlink is rejected too.
func openFileArea(dir string) (*safefs.Root, error) {
	parent, err := safefs.Open(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return parent.Sub(filepath.Base(dir))
}
