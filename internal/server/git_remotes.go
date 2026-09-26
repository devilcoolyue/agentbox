package server

import (
	"net/http"
	"path/filepath"
	"strings"

	"agentbox/internal/gitx"
	"agentbox/internal/store"
)

func (s *Server) handleGitRemoteEdit(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var input struct {
		Repo         string `json:"repo"`
		Name         string `json:"name"`
		Action       string `json:"action"`
		URL          string `json:"url"`
		ExpectedURL  string `json:"expected_url"`
		ConnectionID string `json:"connection_id"`
	}
	if !decodeGitJSON(w, r, &input) {
		return
	}
	if !gitRemoteName.MatchString(input.Name) || (input.Action != "add" && input.Action != "update" && input.Action != "remove") {
		writeErr(w, 400, "远程名称或操作无效")
		return
	}
	dir, ok := s.gitRepo(w, sess, input.Repo)
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
	rel, err := filepath.Rel(s.workspaceDir(sess), dir)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	if rel == "." {
		rel = ""
	}
	input.Repo = filepath.ToSlash(rel)
	bindings, err := s.store.GitBindings(sess.ID)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	// A bound remote is an explicit credential destination. Requiring unbind first
	// keeps changing Git config separate from granting access to a new repository.
	for _, b := range bindings {
		if b.Repo == input.Repo && b.Remote == input.Name {
			writeErr(w, 409, "请先解除此 remote 的连接绑定，再修改或删除地址")
			return
		}
	}
	old, err := s.runGit(ctx, sess, dir, "config", "--local", "--null", "--get-all", "remote."+input.Name+".url")
	exists := err == nil
	if err != nil && !gitx.IsExit(err, 1) {
		writeGitErr(w, err)
		return
	}
	if input.Action == "add" && exists {
		writeErr(w, 409, "远程名称已存在，请刷新后编辑")
		return
	}
	if input.Action != "add" {
		values := strings.Split(strings.TrimSuffix(old, "\x00"), "\x00")
		if !exists || len(values) != 1 || displayGitURL(values[0]) != input.ExpectedURL {
			writeErr(w, 409, "远程地址已变化或包含多个地址，请刷新后核实")
			return
		}
	}
	var args []string
	if input.Action == "remove" {
		args = []string{"remote", "remove", input.Name}
	} else {
		c, err := s.store.GitConnectionFor(sess.User, input.ConnectionID)
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
		if !c.Enabled {
			writeErr(w, 409, "连接已停用")
			return
		}
		target, err := gitRepositoryURL(input.URL, c)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		// Avoid retaining a different push destination behind a newly changed fetch
		// URL. Users can delete and recreate multi-destination remotes explicitly.
		if input.Action == "update" {
			push, err := s.runGit(ctx, sess, dir, "config", "--local", "--null", "--get-all", "remote."+input.Name+".pushurl")
			if err != nil && !gitx.IsExit(err, 1) {
				writeGitErr(w, err)
				return
			}
			if push != "" {
				writeErr(w, 409, "此 remote 有独立 pushurl，请在终端处理或删除后重建")
				return
			}
			args = []string{"remote", "set-url", input.Name, target}
		} else {
			args = []string{"remote", "add", input.Name, target}
		}
	}
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, input.Repo, "", "remote."+input.Action, input.Name)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	if _, err = s.runGit(ctx, sess, dir, args...); err != nil {
		writeErr(w, 409, "远程配置失败，请刷新仓库状态后重试")
		return
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"ok": true, "operation_id": op})
}
