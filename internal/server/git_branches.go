package server

import (
	"net/http"
	"strings"

	"agentbox/internal/store"
)

type gitBranch struct {
	Name     string `json:"name"`
	Head     string `json:"head"`
	Upstream string `json:"upstream"`
	Current  bool   `json:"current"`
	Remote   bool   `json:"remote"`
}

func (s *Server) handleGitBranches(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, ok := s.gitRepo(w, sess, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	ctx, cancel := gitCtx(r)
	defer cancel()
	output, err := s.runGit(ctx, sess, dir, "for-each-ref", "--count=501", "--format=%(refname)%00%(objectname)%00%(upstream:short)%00%(HEAD)%00%(symref)", "refs/heads/", "refs/remotes/")
	if err != nil {
		writeGitErr(w, err)
		return
	}
	rows := []gitBranch{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Split(line, "\x00")
		if len(parts) != 5 || parts[4] != "" {
			continue
		}
		remote := strings.HasPrefix(parts[0], "refs/remotes/")
		name := strings.TrimPrefix(parts[0], "refs/heads/")
		if remote {
			name = strings.TrimPrefix(parts[0], "refs/remotes/")
		}
		rows = append(rows, gitBranch{Name: name, Head: parts[1], Upstream: parts[2], Current: parts[3] == "*", Remote: remote})
	}
	truncated := len(rows) > 500
	if truncated {
		rows = rows[:500]
	}
	status, err := s.runGit(ctx, sess, dir, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		writeGitErr(w, err)
		return
	}
	branch, files := parseGitState(status)
	writeJSON(w, 200, map[string]any{"branches": rows, "state": branch, "dirty": len(files) > 0, "truncated": truncated})
}
func (s *Server) handleGitBranchAction(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		Repo           string `json:"repo"`
		Action         string `json:"action"`
		Name           string `json:"name"`
		Start          string `json:"start"`
		ExpectedHead   string `json:"expected_head"`
		ExpectedBranch string `json:"expected_branch"`
		TargetHead     string `json:"target_head"`
	}
	if !decodeGitJSON(w, r, &req) {
		return
	}
	if req.Action != "create" && req.Action != "switch" && req.Action != "delete" && req.Action != "upstream" {
		writeErr(w, 400, "不支持的分支操作")
		return
	}
	dir, ok := s.gitRepo(w, sess, req.Repo)
	if !ok {
		return
	}
	ctx, cancel := gitCtx(r)
	defer cancel()
	unlock, err := s.lockGit(ctx, sess, dir)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	defer unlock()
	status, err := s.runGit(ctx, sess, dir, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		writeGitErr(w, err)
		return
	}
	branch, files := parseGitState(status)
	if branch.Head != req.ExpectedHead || branch.Branch != req.ExpectedBranch {
		writeErr(w, 409, "当前分支已变化，请刷新后重试")
		return
	}
	if branch.Unborn {
		writeErr(w, 409, "请先创建首次提交再管理分支")
		return
	}
	if !gitText(req.Name, 240, false) || strings.HasPrefix(req.Name, "-") || strings.HasPrefix(req.Name, "refs/") {
		writeErr(w, 400, "分支名无效")
		return
	}
	if _, err = s.runGit(ctx, sess, dir, "check-ref-format", "refs/heads/"+req.Name); err != nil {
		writeErr(w, 400, "分支名不符合 Git 规则")
		return
	}
	if (req.Action == "switch" || req.Action == "create") && len(files) > 0 {
		writeErr(w, 409, "切换或创建分支前，请先提交或处理工作区改动")
		return
	}
	targetRef := "refs/heads/" + req.Name
	args := []string{}
	switch req.Action {
	case "create":
		start := branch.Head
		if req.Start != "" {
			if !strings.HasPrefix(req.Start, "refs/heads/") && !strings.HasPrefix(req.Start, "refs/remotes/") {
				writeErr(w, 400, "起点必须是已存在的本地或远程跟踪分支")
				return
			}
			if _, err = s.runGit(ctx, sess, dir, "check-ref-format", req.Start); err != nil {
				writeErr(w, 400, "起点分支无效")
				return
			}
			out, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", req.Start+"^{commit}")
			if err != nil || strings.TrimSpace(out) != req.TargetHead {
				writeErr(w, 409, "起点分支已变化，请刷新后重试")
				return
			}
			start = strings.TrimSpace(out)
		}
		args = []string{"switch", "--no-guess", "--no-track", "-c", req.Name, start}
	case "switch", "delete":
		out, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", targetRef+"^{commit}")
		if err != nil || strings.TrimSpace(out) != req.TargetHead {
			writeErr(w, 409, "目标分支已变化或不存在，请刷新")
			return
		}
		if req.Action == "switch" {
			args = []string{"switch", "--no-guess", req.Name}
		} else {
			if branch.Branch == req.Name {
				writeErr(w, 409, "不能删除当前分支")
				return
			}
			// Require ancestry to current HEAD as well as Git -d's upstream guard;
			// never offer force deletion or silently discard unique local commits.
			if _, err = s.runGit(ctx, sess, dir, "merge-base", "--is-ancestor", req.TargetHead, branch.Head); err != nil {
				writeErr(w, 409, "目标分支包含当前分支尚未合并的提交，不能删除")
				return
			}
			args = []string{"branch", "-d", "--", req.Name}
		}
	case "upstream":
		if branch.Detached {
			writeErr(w, 409, "游离 HEAD 不能设置上游")
			return
		}
		targetRef = "refs/remotes/" + req.Name
		out, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", targetRef+"^{commit}")
		if err != nil || strings.TrimSpace(out) != req.TargetHead {
			writeErr(w, 409, "远程跟踪分支已变化，请先获取并刷新")
			return
		}
		args = []string{"branch", "--set-upstream-to=" + targetRef, branch.Branch}
	}
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, req.Repo, "", "branch."+req.Action, req.Name)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	if _, err = s.runGit(ctx, sess, dir, args...); err != nil {
		writeErr(w, 409, "分支操作失败：请检查分支是否已存在、在其他 worktree 使用或含未合并提交；可到终端查看 Git 状态")
		return
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"ok": true, "operation_id": op})
}
