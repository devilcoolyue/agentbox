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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentbox/internal/archivex"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// runGit runs git in dir with ownership checks disabled (root reads a repo owned
// by uid 1000). Returns stdout; on failure the error carries stderr.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-c", "safe.directory=" + dir, "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
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

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ws := s.workspaceDir(sess)
	ctx, cancel := gitCtx(r)
	defer cancel()
	if _, err := runGit(ctx, ws, "rev-parse", "--is-inside-work-tree"); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"is_repo": false})
		return
	}
	branch, _ := runGit(ctx, ws, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := runGit(ctx, ws, "status", "--porcelain=v1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"is_repo": true,
		"branch":  strings.TrimSpace(branch),
		"files":   parsePorcelain(out),
	})
}

// handleGitDiff streams a unified diff (all changes vs HEAD, or one file). Plain
// text; the client renders it. Untracked files have no HEAD diff — the client
// previews those through the file API instead.
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ws := s.workspaceDir(sess)
	ctx, cancel := gitCtx(r)
	defer cancel()
	args := []string{"diff", "HEAD"}
	if path := filepath.FromSlash(r.URL.Query().Get("path")); path != "" {
		if !filepath.IsLocal(path) {
			writeErr(w, http.StatusBadRequest, "invalid path")
			return
		}
		args = append(args, "--", path)
	}
	out, err := runGit(ctx, ws, args...)
	if err != nil {
		// A repo with no commits yet has no HEAD; fall back to diffing the index.
		fallback := append([]string{"diff"}, args[1:]...)
		out, _ = runGit(ctx, ws, fallback...)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

func (s *Server) chownWorkspace(sess store.Session) {
	// Best-effort: keep everything git touched owned by the container user.
	_ = archivex.ChownTree(s.workspaceDir(sess), dockerx.AgentUID, dockerx.AgentGID)
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		Message string `json:"message"`
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
	ws := s.workspaceDir(sess)
	ctx, cancel := gitCtx(r)
	defer cancel()
	if _, err := runGit(ctx, ws, "add", "-A"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := runGit(ctx, ws,
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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ws := s.workspaceDir(sess)
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
	_, _ = runGit(ctx, ws, "checkout", "HEAD", "--", target)
	if _, err := runGit(ctx, ws, "clean", "-fd", "--", target); err != nil {
		s.chownWorkspace(sess)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.chownWorkspace(sess)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
