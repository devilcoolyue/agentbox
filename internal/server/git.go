// Git 变更审查：对会话 workspace 直接跑宿主机 git，让用户在网页里查看 agent 的
// 改动、提交或回滚，无需切到终端。写操作后把 .git/工作树属主修回容器用户
// (1000:1000)，避免 root 写下的对象让容器内 agent 后续 git 操作失权。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentbox/internal/archivex"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// runGit runs git in dir with ownership checks disabled (root reads a repo owned
// by uid 1000). Returns stdout; on failure the error carries stderr.
//
// dir must be a directory we already know holds a .git: session workspaces live
// under data_dir, which may itself sit inside a git repo (the server's own
// checkout). Left to itself git's repo discovery walks up out of the workspace
// and lands on that repo, so every session would report the server repo's
// changes — and `add -A` would stage its whole tree. GIT_CEILING_DIRECTORIES
// stops the walk one level above dir; git ignores relative entries, hence Abs.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	// core.excludesFile is not covered by GIT_CONFIG_GLOBAL: its default path
	// (~/.config/git/ignore) applies even with no global config at all, so it
	// has to be pointed at an empty file explicitly. The repo's own .gitignore
	// and .git/info/exclude still apply — those belong to the repo.
	full := append([]string{
		"-c", "safe.directory=" + dir,
		"-c", "core.excludesFile=/dev/null",
		"-C", dir,
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	// Review must reflect the repo's own rules, not whoever's config the server
	// inherited: root's ~/.config/git/ignore would silently hide files from the
	// user (Claude Code's own default ignores .claude/settings.local.json), and
	// core.hooksPath there would run host hooks on a container-owned repo. That
	// leak only bites when HOME happens to be set — systemd gives the unit none,
	// a shell does — which is exactly the kind of difference not worth debugging.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if abs, err := filepath.Abs(dir); err == nil {
		cmd.Env = append(cmd.Env, "GIT_CEILING_DIRECTORIES="+filepath.Dir(abs))
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
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

// hasGitDir reports whether dir is a repository root. .git is a directory for a
// normal clone and a file for worktrees/submodules, so only existence matters.
func hasGitDir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// repoRoots lists the repositories a session's change review may act on, as
// slash paths relative to ws ("" = ws itself). The workspace root is rarely a
// repo in practice — users clone or unzip a project into a subdirectory — so
// when it isn't we look inside instead of giving up. Symlinked directories are
// skipped (ReadDir reports the link's own type), keeping the scan inside ws.
func repoRoots(ws string) []string {
	if hasGitDir(ws) {
		return []string{""}
	}
	out := []string{}
	var walk func(dir, rel string, depth int)
	walk = func(dir, rel string, depth int) {
		if depth > repoScanDepth || len(out) >= repoScanMax {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || repoScanSkip[e.Name()] {
				continue
			}
			sub, subRel := filepath.Join(dir, e.Name()), path.Join(rel, e.Name())
			if hasGitDir(sub) {
				// Repos nested inside a repo are that repo's own business.
				out = append(out, subRel)
			} else {
				walk(sub, subRel, depth+1)
			}
			if len(out) >= repoScanMax {
				return
			}
		}
	}
	walk(ws, "", 1)
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
	roots := repoRoots(ws)
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
	roots := repoRoots(ws)
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
	branch, _ := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	// --untracked-files=all: the default collapses a wholly-untracked directory
	// into one "dir/" entry, so a new .claude/settings.local.json shows up as
	// ".claude/" with no way to diff or discard the file itself.
	out, err := runGit(ctx, dir, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
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
	ctx, cancel := gitCtx(r)
	defer cancel()
	args := []string{"diff", "HEAD"}
	if p := filepath.FromSlash(r.URL.Query().Get("path")); p != "" {
		if !filepath.IsLocal(p) {
			writeErr(w, http.StatusBadRequest, "invalid path")
			return
		}
		args = append(args, "--", p)
	}
	out, err := runGit(ctx, dir, args...)
	if err != nil {
		// A repo with no commits yet has no HEAD; fall back to diffing the index.
		fallback := append([]string{"diff"}, args[1:]...)
		out, _ = runGit(ctx, dir, fallback...)
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
	p, err := resolveUnderRoot(dir, r.URL.Query().Get("path"))
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	info, err := os.Lstat(p)
	if err != nil {
		// Deleted files are in the change list but no longer on disk.
		writeErr(w, http.StatusNotFound, "文件已不存在，只能看 diff")
		return
	}
	if !info.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "不是普通文件")
		return
	}
	if info.Size() > maxFileViewBytes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("文件太大（%.1f MB），请到「文件」页打开", float64(info.Size())/(1<<20)))
		return
	}
	body, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if bytes.IndexByte(body, 0) >= 0 || !utf8.Valid(body) {
		writeErr(w, http.StatusBadRequest, "二进制文件，无法按文本查看")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *Server) chownWorkspace(sess store.Session) {
	// Best-effort: keep everything git touched owned by the container user.
	_ = archivex.ChownTree(s.workspaceDir(sess), dockerx.AgentUID, dockerx.AgentGID)
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
	if _, err := runGit(ctx, dir, "add", "-A"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := runGit(ctx, dir,
		"-c", "user.name=agentbox", "-c", "user.email=agentbox@localhost",
		"commit", "-m", msg)
	s.chownWorkspace(sess)
	if err != nil {
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
	// checkout restores tracked files; clean removes untracked ones. A path that
	// is only untracked makes checkout error, which is fine — clean handles it.
	_, _ = runGit(ctx, dir, "checkout", "HEAD", "--", target)
	if _, err := runGit(ctx, dir, "clean", "-fd", "--", target); err != nil {
		s.chownWorkspace(sess)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.chownWorkspace(sess)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
