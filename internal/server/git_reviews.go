package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
)

func (s *Server) gitForge(ctx context.Context, sess store.Session, dir, remote, apiConnection string, write bool) (*gitaccess.Forge, store.GitConnection, store.GitBinding, func(), error) {
	c, b, err := s.gitBoundConnection(ctx, sess, dir, remote, false)
	if err != nil {
		return nil, c, b, nil, err
	}
	repository := b.URL
	if c.AuthType == "ssh" {
		if apiConnection == "" {
			return nil, c, b, nil, errors.New("SSH 仅用于 Git 传输，请另选此平台的 HTTPS Token/OAuth 连接访问 PR/MR API")
		}
		api, err := s.store.GitConnectionFor(sess.User, apiConnection)
		if err != nil {
			return nil, c, b, nil, errors.New("平台 API 连接不存在或不可用")
		}
		sshURL, _ := url.Parse(repository)
		base, _ := url.Parse(api.BaseURL)
		if api.Provider != c.Provider || api.AuthType == "ssh" || sshURL.Hostname() != base.Hostname() {
			return nil, c, b, nil, errors.New("API 连接必须属于同一平台和主机的 HTTPS 服务")
		}
		sshBase, _ := url.Parse(c.BaseURL)
		repository = strings.TrimRight(api.BaseURL, "/") + "/" + strings.TrimPrefix(strings.TrimPrefix(sshURL.Path, sshBase.Path), "/")
		c = api
	} else if apiConnection != "" && apiConnection != c.ID {
		return nil, c, b, nil, errors.New("HTTPS 仓库请使用其已绑定连接访问 API")
	}
	if !c.Enabled || write && c.ReadOnly {
		return nil, c, b, nil, errors.New("API 连接已停用或只读，无法创建合并请求")
	}
	current, token, err := s.gitCredential(ctx, c)
	if err != nil {
		return nil, c, b, nil, err
	}
	c = current
	if write && c.ReadOnly {
		return nil, c, b, nil, errors.New("API 连接已改为只读")
	}
	client, closeClient, err := s.gitClient(ctx, c)
	if err != nil {
		return nil, c, b, nil, err
	}
	forge, err := gitaccess.NewForge(c.Provider, c.BaseURL, repository, token, c.AuthType, client)
	if err != nil {
		closeClient()
		return nil, c, b, nil, err
	}
	return forge, c, b, closeClient, nil
}
func (s *Server) handleGitReviews(w http.ResponseWriter, r *http.Request, sess store.Session) {
	q := r.URL.Query()
	dir, ok := s.gitRepo(w, sess, q.Get("repo"))
	if !ok {
		return
	}
	page := 1
	if raw := q.Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 1000 {
			writeErr(w, 400, "页码无效")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	forge, c, b, closeClient, err := s.gitForge(ctx, sess, dir, q.Get("remote"), q.Get("api_connection_id"), false)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	defer closeClient()
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, b.Repo, c.ID, "review.list", b.Remote)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	rows, more, err := forge.Reviews(ctx, "", "", page)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defaultBranch, err := forge.DefaultBranch(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	branch, err := s.runGit(ctx, sess, dir, "symbolic-ref", "--short", "--quiet", "HEAD")
	if err != nil {
		branch = ""
	}
	head, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		head = ""
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"provider": c.Provider, "project": forge.Project, "connection_id": c.ID, "read_only": c.ReadOnly, "rows": rows, "has_more": more, "page": page, "default_branch": defaultBranch, "source_branch": strings.TrimSpace(branch), "head": strings.TrimSpace(head)})
}

type gitReviewRequest struct {
	Repo            string `json:"repo"`
	Remote          string `json:"remote"`
	APIConnectionID string `json:"api_connection_id"`
	Title           string `json:"title"`
	Body            string `json:"body"`
	Source          string `json:"source"`
	Target          string `json:"target"`
	Draft           bool   `json:"draft"`
	ExpectedHead    string `json:"expected_head"`
	ExpectedTarget  string `json:"expected_target"`
}

func (s *Server) handleGitReviewPreview(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.gitReviewWrite(w, r, sess, true)
}
func (s *Server) handleGitReviewCreate(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.gitReviewWrite(w, r, sess, false)
}
func (s *Server) gitReviewWrite(w http.ResponseWriter, r *http.Request, sess store.Session, preview bool) {
	var input gitReviewRequest
	if !decodeGitJSON(w, r, &input) {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	if !gitText(input.Title, 240, false) || len(input.Body) > 32000 || !utf8.ValidString(input.Body) || strings.ContainsRune(input.Body, 0) {
		writeErr(w, 400, "标题或描述无效：标题最多 240 字节，描述最多 32 KiB")
		return
	}
	if input.Source == input.Target || !gitText(input.Source, 240, false) || !gitText(input.Target, 240, false) {
		writeErr(w, 400, "请选择不同的有效来源与目标分支")
		return
	}
	dir, ok := s.gitRepo(w, sess, input.Repo)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	unlock, err := s.lockGit(ctx, sess, dir)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	defer unlock()
	for _, branch := range []string{input.Source, input.Target} {
		if _, err = s.runGit(ctx, sess, dir, "check-ref-format", "refs/heads/"+branch); err != nil {
			writeErr(w, 400, "分支名不符合 Git 规则")
			return
		}
	}
	head, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(head) != input.ExpectedHead {
		writeErr(w, 409, "本地提交已变化，请重新刷新预览")
		return
	}
	branch, err := s.runGit(ctx, sess, dir, "symbolic-ref", "--short", "--quiet", "HEAD")
	if err != nil || strings.TrimSpace(branch) != input.Source {
		writeErr(w, 409, "当前分支与来源分支不同，请重新刷新")
		return
	}
	forge, c, b, closeClient, err := s.gitForge(ctx, sess, dir, input.Remote, input.APIConnectionID, true)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	defer closeClient()
	operation := "review.create"
	if preview {
		operation = "review.preview"
	}
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, b.Repo, c.ID, operation, input.Source+" -> "+input.Target)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	source, err := forge.Branch(ctx, input.Source)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if source.SHA != input.ExpectedHead {
		writeErr(w, 409, "远程来源分支与本地提交不同，请先推送并重新预览")
		return
	}
	target, err := forge.Branch(ctx, input.Target)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if !preview && target.SHA != input.ExpectedTarget {
		writeErr(w, 409, "远程目标分支已变化，请重新预览")
		return
	}
	if source.SHA == target.SHA {
		writeErr(w, 409, "来源与目标提交相同，无需创建合并请求")
		return
	}
	existing, _, err := forge.Reviews(ctx, input.Source, input.Target, 1)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if preview {
		result = "success"
		writeJSON(w, 200, map[string]any{"provider": c.Provider, "project": forge.Project, "connection_id": c.ID, "source": source, "target": target, "title": input.Title, "body": input.Body, "draft": input.Draft, "existing": existing})
		return
	}
	if len(existing) > 0 {
		result = "success"
		writeJSON(w, 200, map[string]any{"review": existing[0], "existing": true, "operation_id": op})
		return
	}
	// Recheck authorization directly before the outbound mutation. Never retry a
	// POST on transport failure; a subsequent list can resolve unknown outcomes.
	current, err := s.store.GitConnectionFor(gitActor(c), c.ID)
	if err != nil || !current.Enabled || current.ReadOnly || current.Revision != c.Revision {
		writeErr(w, 409, "API 连接已变化，请重新预览")
		return
	}
	review, err := forge.CreateReview(ctx, gitaccess.ReviewInput{Title: input.Title, Body: input.Body, Source: input.Source, Target: input.Target, Draft: input.Draft})
	if err != nil {
		result = "failed_unknown"
		writeErr(w, 502, err.Error()+"；创建结果可能未知，请先刷新平台列表核实")
		return
	}
	if review.Number <= 0 || review.URL == "" {
		result = "failed_unknown"
		writeErr(w, 502, "平台可能已创建请求，但返回信息不完整，请刷新列表核实")
		return
	}
	result = "success"
	writeJSON(w, 201, map[string]any{"review": review, "existing": false, "operation_id": op})
}
