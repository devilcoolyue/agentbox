package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"agentbox/internal/archivex"
	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

type projectImportResult struct {
	Directory string `json:"directory"`
	Files     int    `json:"files,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

func importFingerprint(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) finishProjectImport(w http.ResponseWriter, r *http.Request, c store.WorkspaceCreation, attempt store.WorkspaceImport, state string, result projectImportResult) {
	raw, _ := json.Marshal(result)
	if err := s.store.FinishWorkspaceImport(reqUser(r), c.RequestID, attempt.AttemptID, state, raw); err != nil {
		// A failed acknowledgement cannot prove publication rolled back.
		writeCreationError(w, r, store.ErrImportPending)
		return
	}
	attempt.State = state
	attempt.Result = raw
	writeJSON(w, 200, attempt)
}

func (s *Server) targetAvailable(w http.ResponseWriter, r *http.Request, sess store.Session, name string) bool {
	root, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		writeCreationError(w, r, err)
		return false
	}
	defer root.Close()
	_, err = root.Lstat(name)
	if err == nil {
		writeProblem(w, r, "project.import", "import_target_exists")
		return false
	}
	if !errors.Is(err, fs.ErrNotExist) {
		writeCreationError(w, r, err)
		return false
	}
	return true
}

// Verify the complete input fingerprint even on a replay. A reused ID with
// different files or a different repository must not claim the old result.
func (s *Server) importReplay(w http.ResponseWriter, r *http.Request, c store.WorkspaceCreation, id, fingerprint string) bool {
	item, err := s.store.WorkspaceImport(reqUser(r), c.RequestID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		writeCreationError(w, r, err)
		return true
	}
	if item.Fingerprint != fingerprint {
		writeCreationError(w, r, store.ErrCreationConflict)
		return true
	}
	if item.State == "running" {
		item.State = "uncertain"
	}
	writeJSON(w, 200, item)
	return true
}

// Files are staged outside container mounts, validated, then atomically
// published into a new directory. Never merge or clear an existing project.
func (s *Server) handleProjectUpload(w http.ResponseWriter, r *http.Request) {
	c, sess, ok := s.importCreation(w, r)
	if !ok {
		return
	}
	attemptID := r.URL.Query().Get("attempt_id")
	directory := r.URL.Query().Get("directory")
	if !operationIDPattern.MatchString(attemptID) || !validProjectDirectory(directory) {
		writeProblem(w, r, "project.import", "invalid_request")
		return
	}
	if !s.beginCreationWork(sess.ID) {
		writeCreationError(w, r, store.ErrImportPending)
		return
	}
	defer s.endCreationWork(sess.ID)
	maxMB := s.cfg.GetMaxUploadMB()
	if maxMB < 1 || maxMB > 1<<30 {
		writeProblem(w, r, "project.import", "invalid_request")
		return
	}
	maxBytes := maxMB << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, r, "project.import", "project_upload_limits")
			return
		}
		writeProblem(w, r, "project.import", "invalid_request")
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["files"]
	if len(files) == 0 || len(files) > 2000 {
		writeProblem(w, r, "project.import", "invalid_request")
		return
	}
	remaining := maxBytes
	for _, file := range files {
		if file.Size < 0 || file.Size > remaining {
			writeProblem(w, r, "project.import", "project_upload_limits")
			return
		}
		remaining -= file.Size
	}
	mode := r.FormValue("mode")
	var paths []string
	if mode == "archive" {
		if len(files) != 1 || !archivex.IsArchiveName(files[0].Filename) {
			writeProblem(w, r, "project.import", "invalid_request")
			return
		}
		paths = []string{files[0].Filename}
	} else if mode == "files" {
		if json.Unmarshal([]byte(r.FormValue("paths")), &paths) != nil || len(paths) != len(files) {
			writeProblem(w, r, "project.import", "invalid_request")
			return
		}
		seen := map[string]bool{}
		for _, name := range paths {
			if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00") || seen[name] {
				writeProblem(w, r, "project.import", "invalid_request")
				return
			}
			seen[name] = true
		}
	} else {
		writeProblem(w, r, "project.import", "invalid_request")
		return
	}
	stage, err := os.MkdirTemp(s.cfg.DataDir, ".stage-project-*")
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	defer os.RemoveAll(stage)
	content := filepath.Join(stage, "content")
	if err = os.Mkdir(content, 0700); err != nil {
		writeCreationError(w, r, err)
		return
	}
	staged, err := safefs.Open(content)
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	defer staged.Close()
	hash := sha256.New()
	sizes := make([]int64, len(files))
	for i, file := range files {
		sizes[i] = file.Size
	}
	metadata, _ := json.Marshal(struct {
		Mode, Directory string
		Paths           []string
		Sizes           []int64
	}{mode, directory, paths, sizes})
	hash.Write(metadata)
	count := 0
	for i, hdr := range files {
		if r.Context().Err() != nil {
			return
		}
		file, err := hdr.Open()
		if err != nil {
			writeProblem(w, r, "project.import", "import_failed")
			return
		}
		if mode == "archive" {
			archive := filepath.Join(stage, "archive")
			out, openErr := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if openErr == nil {
				_, err = io.Copy(io.MultiWriter(out, hash), file)
				err = errors.Join(err, out.Close())
			} else {
				err = openErr
			}
			file.Close()
			if err == nil {
				count, err = archivex.ExtractRoot(archive, hdr.Filename, staged, maxBytes*archivex.ExtractLimitMultiplier)
			}
		} else {
			parent := path.Dir(paths[i])
			if parent != "." {
				err = staged.MkdirAll(parent, 0755)
			}
			if err == nil {
				_, err = staged.WriteAtomic(paths[i], io.TeeReader(file, hash), safefs.WriteOptions{Mode: 0644, MaxBytes: maxBytes, Limit: true})
			}
			file.Close()
			count++
		}
		if err != nil {
			writeProblem(w, r, "project.import", "import_failed")
			return
		}
	}
	if err = archivex.ChownRoot(staged, dockerx.AgentUID, dockerx.AgentGID); err != nil && runtime.GOOS == "linux" {
		writeCreationError(w, r, err)
		return
	}
	attempt := store.WorkspaceImport{AttemptID: attemptID, Kind: "upload", Directory: directory, Fingerprint: hex.EncodeToString(hash.Sum(nil))}
	if s.importReplay(w, r, c, attemptID, attempt.Fingerprint) {
		return
	}
	if !s.targetAvailable(w, r, sess, directory) {
		return
	}
	// Protect publication against explicit workspace removal, using the same lock
	// as every normal lifecycle action. Staging above is outside the workspace.
	err = s.workspaces().WithSession(r.Context(), sess.ID, func(current store.Session) error {
		if current.User != reqUser(r).Name {
			return store.ErrCreationGone
		}
		attempt, fresh, err := s.store.BeginWorkspaceImport(reqUser(r), c.RequestID, attempt)
		if err != nil {
			return err
		}
		if !fresh {
			writeJSON(w, 200, attempt)
			return nil
		}
		result := projectImportResult{Directory: directory, Files: count}
		area, err := s.openDataDir(s.workspaceDir(current))
		if err != nil {
			s.finishProjectImport(w, r, c, attempt, "failed", result)
			return nil
		}
		defer area.Close()
		if r.Context().Err() != nil {
			s.finishProjectImport(w, r, c, attempt, "failed", result)
			return nil
		}
		stageRoot, err := safefs.Open(stage)
		if err != nil {
			s.finishProjectImport(w, r, c, attempt, "failed", result)
			return nil
		}
		defer stageRoot.Close()
		if err = stageRoot.RenameTo("content", area, directory, false); err != nil {
			// A publication error or competing target needs explicit inspection.
			s.finishProjectImport(w, r, c, attempt, "uncertain", result)
			return nil
		}
		if err = area.Sync(); err != nil {
			s.finishProjectImport(w, r, c, attempt, "uncertain", result)
			return nil
		}
		s.finishProjectImport(w, r, c, attempt, "succeeded", result)
		return nil
	})
	if err != nil {
		writeCreationError(w, r, err)
	}
}

func (s *Server) handleProjectGit(w http.ResponseWriter, r *http.Request) {
	c, sess, ok := s.importCreation(w, r)
	if !ok {
		return
	}
	var input struct {
		AttemptID    string `json:"attempt_id"`
		ConnectionID string `json:"connection_id"`
		URL          string `json:"url"`
		Directory    string `json:"directory"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input) != nil || !operationIDPattern.MatchString(input.AttemptID) || !validProjectDirectory(input.Directory) {
		writeProblem(w, r, "project.import", "invalid_request")
		return
	}
	if !s.beginCreationWork(sess.ID) {
		writeCreationError(w, r, store.ErrImportPending)
		return
	}
	defer s.endCreationWork(sess.ID)
	if _, err := s.sessionAccount(sess); err != nil {
		writeCreationError(w, r, err)
		return
	}
	if s.quotaBlock(sess.User) != "" {
		writeProblem(w, r, "project.import", "quota_exhausted")
		return
	}
	connection, err := s.store.GitConnectionFor(sess.User, input.ConnectionID)
	if err != nil || !connection.Enabled {
		writeProblem(w, r, "project.import", "import_failed")
		return
	}
	repository, err := gitRepositoryURL(input.URL, connection)
	if err != nil {
		writeProblem(w, r, "project.import", "invalid_request")
		return
	}

	if s.git == nil {
		writeProblem(w, r, "project.import", "import_failed")
		return
	}
	fingerprint := importFingerprint([]string{"git", connection.ID, repository, input.Directory})
	if s.importReplay(w, r, c, input.AttemptID, fingerprint) {
		return
	}
	if !s.targetAvailable(w, r, sess, input.Directory) {
		return
	}
	attempt, fresh, err := s.store.BeginWorkspaceImport(reqUser(r), c.RequestID, store.WorkspaceImport{AttemptID: input.AttemptID, Kind: "git", Directory: input.Directory, Fingerprint: fingerprint})
	if err != nil {
		writeCreationError(w, r, err)
		return
	}
	if !fresh {
		writeJSON(w, 200, attempt)
		return
	}
	body, _ := json.Marshal(map[string]string{"connection_id": connection.ID, "url": repository, "directory": input.Directory})
	copy := r.Clone(r.Context())
	copy.Body = io.NopCloser(bytes.NewReader(body))
	copy.ContentLength = int64(len(body))
	copy.Header = copy.Header.Clone()
	copy.Header.Set("X-Git-Request-ID", input.AttemptID)
	copy.SetPathValue("id", sess.ID)
	captured := &importResponse{}
	s.gitOperation("clone", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleGitClone(w, r, sess) })).ServeHTTP(captured, copy)
	result := projectImportResult{Directory: input.Directory}
	state := "failed"
	if captured.status >= 200 && captured.status < 300 && !captured.overflow {
		var response struct{ Repo, Warning string }
		if json.Unmarshal(captured.body.Bytes(), &response) == nil && response.Repo == input.Directory {
			state = "succeeded"
			if response.Warning != "" {
				result.Warning = "仓库已导入，请在 Git 设置中检查连接绑定。"
			}
		} else {
			state = "uncertain"
		}
	} else {
		root, err := s.openDataDir(s.workspaceDir(sess))
		if err != nil {
			state = "uncertain"
		} else {
			_, err = root.Lstat(input.Directory)
			root.Close()
			if !errors.Is(err, fs.ErrNotExist) {
				state = "uncertain"
			}
		}
	}
	s.finishProjectImport(w, r, c, attempt, state, result)
}
