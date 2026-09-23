// Git 变更审查：目录发现与 HTTP 响应在这里，Git 命令交给 gitx 在会话容器内
// 以 1000:1000 执行。仓库配置可执行代码，因此绝不能回退宿主机 Git。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentbox/internal/gitx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

var errGitQuota = errors.New("额度已用完，无法运行 Git；文件仍可查看和下载")

func (s *Server) prepareGitSession(ctx context.Context, id string) (string, func(), error) {
	sess, ok := s.store.Get(id)
	if !ok {
		return "", nil, errSessionGone
	}
	// Git filters are executable user code too. Apply the same entry gate as
	// terminal access before starting a container or invoking any Git command.
	if s.quotaBlock(sess.User) != "" {
		return "", nil, errGitQuota
	}
	s.idle.hold(id)
	release := func() { s.idle.release(id) }
	sess, err := s.startSession(ctx, sess)
	if err != nil {
		release()
		return "", nil, err
	}
	return sess.ContainerID, release, nil
}

func (s *Server) runGit(ctx context.Context, sess store.Session, dir string, args ...string) (string, error) {
	rel, err := filepath.Rel(s.workspaceDir(sess), dir)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("invalid Git repository path")
	}
	return s.git.Run(ctx, sess.ID, filepath.ToSlash(rel), args...)
}

func writeGitErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, errGitQuota) {
		status = http.StatusForbidden
	} else if errors.Is(err, gitx.ErrUnavailable) {
		status = http.StatusServiceUnavailable
	}
	writeErr(w, status, err.Error())
}

// 扫描 workspace 找仓库的边界：往下最多两层（上传的压缩包常多包一层目录），
// 结果上限 20 个，跳过隐藏目录和这些一定不是项目根的目录。
const (
	repoScanDepth = 2
	repoScanMax   = 20
)

var repoScanSkip = map[string]bool{"node_modules": true, "__MACOSX": true, "vendor": true}

// maxStatusFiles caps the change list: listing untracked files individually
// expands whole untracked trees (an unzipped node_modules nobody gitignored is
// thousands of entries) and the client renders one row per file.
const maxStatusFiles = 2000

// maxFileViewBytes caps the whole-file view. Review reads code, and the pane
// renders one DOM node per line; anything bigger belongs in the files page.
const maxFileViewBytes = 512 << 10

// repoRoots discovers only real directories through data_dir's pinned root.
func (s *Server) repoRoots(ws string) []string {
	root, err := s.openDataDir(ws)
	if err != nil {
		return []string{}
	}
	defer root.Close()
	hasGit := func(dir *safefs.Root) bool {
		info, err := dir.Lstat(".git")
		return err == nil && (info.IsDir() || info.Mode().IsRegular())
	}
	if hasGit(root) {
		return []string{""}
	}
	out := []string{}
	var walk func(*safefs.Root, string, int)
	walk = func(dir *safefs.Root, rel string, depth int) {
		if depth > repoScanDepth || len(out) >= repoScanMax {
			return
		}
		entries, err := dir.ReadDir(".")
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || repoScanSkip[e.Name()] {
				continue
			}
			sub, err := dir.Sub(e.Name())
			if err != nil {
				continue
			}
			subRel := path.Join(rel, e.Name())
			if hasGit(sub) {
				out = append(out, subRel)
			} else {
				walk(sub, subRel, depth+1)
			}
			sub.Close()
			if len(out) >= repoScanMax {
				return
			}
		}
	}
	walk(root, "", 1)
	sort.Strings(out)
	return out
}

// pickRepo resolves the client's requested repo against what we found. An empty
// request means "the default one"; an unknown one is rejected rather than
// silently redirected — commit/discard are destructive and must never land on a
// repo the user did not pick. Callers must pass a non-empty roots.
func pickRepo(roots []string, want string) (string, bool) {
	want = strings.Trim(filepath.ToSlash(want), "/")
	if want == "" {
		return roots[0], true
	}
	for _, r := range roots {
		if r == want {
			return r, true
		}
	}
	return "", false
}

// gitRepo resolves a write request (commit/discard/diff) to an absolute repo
// path, writing the error response itself when there is nothing to act on.
func (s *Server) gitRepo(w http.ResponseWriter, sess store.Session, want string) (string, bool) {
	ws := s.workspaceDir(sess)
	roots := s.repoRoots(ws)
	if len(roots) == 0 {
		writeErr(w, http.StatusBadRequest, "工作区里没有 Git 仓库")
		return "", false
	}
	rel, ok := pickRepo(roots, want)
	if !ok {
		writeErr(w, http.StatusBadRequest, "仓库不存在："+want)
		return "", false
	}
	return filepath.Join(ws, filepath.FromSlash(rel)), true
}

func gitCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

type gitFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"` // porcelain 两字符 XY
	Untracked bool   `json:"untracked"`
}

// parsePorcelain turns `git status --porcelain=v1` lines into entries. Rename
// lines ("R  old -> new") report the new path.
func parsePorcelain(out string) []gitFile {
	files := []gitFile{}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		files = append(files, gitFile{Path: path, Status: xy, Untracked: xy == "??"})
	}
	return files
}

// handleGitStatus reports the repos found in the workspace plus the status of
// the selected one. Unlike the write paths an unknown ?repo= falls back to the
// default: it is a read, and the response carries the authoritative repo list
// for the client to resync a stale selection against.
func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ws := s.workspaceDir(sess)
	roots := s.repoRoots(ws)
	if len(roots) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"is_repo": false, "repos": roots})
		return
	}
	rel, ok := pickRepo(roots, r.URL.Query().Get("repo"))
	if !ok {
		rel = roots[0]
	}
	dir := filepath.Join(ws, filepath.FromSlash(rel))
	ctx, cancel := gitCtx(r)
	defer cancel()
	branch, _ := s.runGit(ctx, sess, dir, "rev-parse", "--abbrev-ref", "HEAD")
	// --untracked-files=all: the default collapses a wholly-untracked directory
	// into one "dir/" entry, so a new .claude/settings.local.json shows up as
	// ".claude/" with no way to diff or discard the file itself.
	out, err := s.runGit(ctx, sess, dir, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		writeGitErr(w, err)
		return
	}
	files := parsePorcelain(out)
	truncated := len(files) > maxStatusFiles
	if truncated {
		files = files[:maxStatusFiles]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"is_repo":   true,
		"repo":      rel,
		"repos":     roots,
		"branch":    strings.TrimSpace(branch),
		"files":     files,
		"truncated": truncated,
	})
}

// handleGitDiff streams a unified diff (all changes vs HEAD, or one file). Plain
// text; the client renders it. Untracked files have no HEAD diff — the client
// reads those through handleGitFile instead. `path` is relative to the repo,
// not to the workspace.
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, ok := s.gitRepo(w, sess, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	p := filepath.FromSlash(r.URL.Query().Get("path"))
	if p != "" && !filepath.IsLocal(p) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	ctx, cancel := gitCtx(r)
	defer cancel()
	args := []string{"diff", "--no-ext-diff", "--no-textconv"}
	// Only exit 1 from this quiet probe means an unborn HEAD. Runtime failures
	// must not become a successful empty diff.
	if _, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		args = append(args, "HEAD")
	} else if gitx.IsExit(err, 1) {
		args = append(args, "--cached")
	} else {
		writeGitErr(w, err)
		return
	}
	args = append(args, "--")
	if p != "" {
		args = append(args, p)
	}
	out, err := s.runGit(ctx, sess, dir, args...)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

// handleGitFile serves a changed file's current working-tree content as plain
// text. A new file has no diff against HEAD at all, and even for a modified one
// the surrounding code is often what review actually needs, so the pane can show
// the whole file instead of the diff. `path` is relative to the repo. Reading
// through the files API would work too, but only after the client re-joined the
// repo path — the git page stays repo-relative end to end.
func (s *Server) handleGitFile(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, ok := s.gitRepo(w, sess, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	root, err := s.openDataDir(dir)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	f, err := root.OpenFile(r.URL.Query().Get("path"))
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
	if info.Size() > maxFileViewBytes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("文件太大（%.1f MB），请到「文件」页打开", float64(info.Size())/(1<<20)))
		return
	}
	body, err := io.ReadAll(io.LimitReader(f, maxFileViewBytes+1))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(body) > maxFileViewBytes {
		writeErr(w, http.StatusBadRequest, "文件太大，请到终端查看")
		return
	}
	if bytes.IndexByte(body, 0) >= 0 || !utf8.Valid(body) {
		writeErr(w, http.StatusBadRequest, "二进制文件，无法按文本查看")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		Message string `json:"message"`
		Repo    string `json:"repo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeErr(w, http.StatusBadRequest, "提交信息不能为空")
		return
	}
	dir, ok := s.gitRepo(w, sess, req.Repo)
	if !ok {
		return
	}
	ctx, cancel := gitCtx(r)
	defer cancel()
	if _, err := s.runGit(ctx, sess, dir, "add", "-A"); err != nil {
		writeGitErr(w, err)
		return
	}
	out, err := s.runGit(ctx, sess, dir,
		"-c", "user.name=agentbox", "-c", "user.email=agentbox@localhost",
		"commit", "-m", msg)
	if err != nil {
		if errors.Is(err, errGitQuota) || errors.Is(err, gitx.ErrUnavailable) {
			writeGitErr(w, err)
			return
		}
		writeErr(w, http.StatusBadRequest, "提交失败："+strings.TrimSpace(out+" "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": strings.TrimSpace(out)})
}

// handleGitDiscard reverts working-tree changes (one path, or all). Tracked
// files are restored to HEAD and untracked files removed — destructive, so the
// client confirms first.
func (s *Server) handleGitDiscard(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		Path string `json:"path"`
		Repo string `json:"repo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	dir, ok := s.gitRepo(w, sess, req.Repo)
	if !ok {
		return
	}
	ctx, cancel := gitCtx(r)
	defer cancel()
	target := "."
	if p := filepath.FromSlash(req.Path); p != "" {
		if !filepath.IsLocal(p) {
			writeErr(w, http.StatusBadRequest, "invalid path")
			return
		}
		target = p
	}
	// Untracked-only selections don't need checkout. For tracked paths, a
	// failed restore must stop the operation before cleaning unrelated files.
	tracked, err := s.runGit(ctx, sess, dir, "ls-files", "-z", "--", target)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	if tracked != "" {
		if _, err := s.runGit(ctx, sess, dir, "checkout", "HEAD", "--", target); err != nil {
			writeGitErr(w, err)
			return
		}
	}
	if _, err := s.runGit(ctx, sess, dir, "clean", "-fd", "--", target); err != nil {
		writeGitErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
