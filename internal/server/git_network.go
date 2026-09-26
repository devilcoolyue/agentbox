package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbox/internal/gitaccess"
	"agentbox/internal/gitx"
	"agentbox/internal/store"
)

func (s *Server) gitClient(ctx context.Context, c store.GitConnection) (*http.Client, func(), error) {
	if s.gitHTTPClient != nil {
		return s.gitHTTPClient(ctx, c)
	}
	tlsConfig, err := c.Network.TLSConfig()
	if err != nil {
		return nil, nil, err
	}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return s.gitDial(ctx, c, network, address)
	}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, tr.CloseIdleConnections, nil
}

func (s *Server) gitTransport(ctx context.Context, c store.GitConnection, repository string, write bool, ref, old, next string) (string, func(), error) {
	if err := s.checkGitTerminalScope(ctx, c, repository, write); err != nil {
		return "", nil, err
	}
	if !c.Enabled || write && c.ReadOnly {
		return "", nil, errors.New("连接已停用或仅允许读取")
	}
	if c.AuthType == "ssh" {
		return s.gitSSHTransport(ctx, c, repository, write, ref, old, next)
	}
	// Decrypt before opening the transport so missing backup keys give a useful
	// control-plane error rather than a generic Git authentication failure.
	var credentialErr error
	c, _, credentialErr = s.gitCredential(ctx, c)
	if credentialErr != nil {
		return "", nil, credentialErr
	}
	pb := s.cfg.GetProxyBridge()
	host, _, err := net.SplitHostPort(pb.Bind)
	if err != nil {
		return "", nil, errors.New("Git 传输网桥地址无效")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", nil, errors.New("Git 传输网桥不可用，请检查 proxy_bridge.bind")
	}
	ticket := make([]byte, 32)
	if _, err = rand.Read(ticket); err != nil {
		ln.Close()
		return "", nil, err
	}
	grant := gitaccess.Grant{Ticket: hex.EncodeToString(ticket), Repository: repository, Write: write, Ref: ref, Old: old, New: next}
	if op := gitLive(ctx); op != nil {
		grant.Progress = func(received, sent int64) { op.read.Add(received); op.written.Add(sent) }
	}
	grant.Authorize = func(ctx context.Context) (string, string, error) {
		if err := s.checkGitTerminalScope(ctx, c, repository, write); err != nil {
			return "", "", err
		}
		current, err := s.store.GitConnectionFor(gitActor(c), c.ID)
		if err != nil || !current.Enabled || current.Revision != c.Revision || write && current.ReadOnly {
			return "", "", errors.New("connection revoked")
		}
		_, token, err := s.gitCredential(ctx, current)
		if err != nil {
			return "", "", err
		}
		return current.Username, token, nil
	}
	client, closeClient, err := s.gitClient(ctx, c)
	if err != nil {
		ln.Close()
		return "", nil, errors.New("Git 网络路由不可用")
	}
	grant.Client = client
	grantCtx, cancelGrant := context.WithCancel(ctx)
	var handlers sync.WaitGroup
	var gate sync.Mutex
	closed := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gate.Lock()
		if closed {
			gate.Unlock()
			http.Error(w, "Git operation finished", 410)
			return
		}
		handlers.Add(1)
		gate.Unlock()
		defer handlers.Done()
		grant.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10, BaseContext: func(net.Listener) context.Context { return grantCtx }}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(ln) }()
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	closeFn := func() {
		stop()
		gate.Lock()
		closed = true
		gate.Unlock()
		cancelGrant()
		_ = server.Close()
		<-done
		handlers.Wait()
		closeClient()
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return "http://" + net.JoinHostPort(pb.Host, fmt.Sprint(port)) + "/" + grant.Ticket, closeFn, nil
}

func (s *Server) runGitNetwork(ctx context.Context, sess store.Session, dir string, args ...string) (string, error) {
	rel, err := filepath.Rel(s.workspaceDir(sess), dir)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("仓库路径无效")
	}
	options := []string{"-c", "protocol.allow=never", "-c", "protocol.http.allow=always", "-c", "protocol.git.allow=always", "-c", "credential.helper=", "-c", "http.extraHeader=", "-c", "http.proxy=", "-c", "http.followRedirects=false", "-c", "fetch.recurseSubmodules=false", "-c", "push.recurseSubmodules=no"}
	gitPhase(ctx, "transferring")
	return s.git.RunNetwork(ctx, sess.ID, filepath.ToSlash(rel), append(options, args...)...)
}

func (s *Server) gitBoundConnection(ctx context.Context, sess store.Session, dir, remote string, write bool) (store.GitConnection, store.GitBinding, error) {
	var empty store.GitConnection
	var binding store.GitBinding
	if !gitRemoteName.MatchString(remote) {
		return empty, binding, errors.New("remote 名称无效")
	}
	rel, err := filepath.Rel(s.workspaceDir(sess), dir)
	if err != nil {
		return empty, binding, err
	}
	if rel == "." {
		rel = ""
	}
	bindings, err := s.store.GitBindings(sess.ID)
	if err != nil {
		return empty, binding, err
	}
	for _, b := range bindings {
		if b.Repo == filepath.ToSlash(rel) && b.Remote == remote {
			binding = b
			break
		}
	}
	if binding.ConnectionID == "" {
		return empty, binding, errors.New("请先为此仓库 remote 绑定 Git 连接")
	}
	c, err := s.store.GitConnectionFor(sess.User, binding.ConnectionID)
	if err != nil {
		return empty, binding, errors.New("绑定的 Git 连接不存在或已撤权")
	}
	if !c.Enabled || write && c.ReadOnly {
		return empty, binding, errors.New("Git 连接已停用或仅允许读取")
	}
	raw, err := s.runGit(ctx, sess, dir, "config", "--local", "--null", "--get-all", "remote."+remote+".url")
	if err != nil {
		return empty, binding, errors.New("无法读取 remote 地址")
	}
	urls := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	if len(urls) != 1 {
		return empty, binding, errors.New("remote 地址已改变，请重新绑定")
	}
	canonical, err := gitRepositoryURL(urls[0], c)
	if err != nil || canonical != binding.URL {
		return empty, binding, errors.New("remote 地址已改变，请重新绑定")
	}
	if write {
		raw, err = s.runGit(ctx, sess, dir, "config", "--local", "--null", "--get-all", "remote."+remote+".pushurl")
		if err != nil && !gitx.IsExit(err, 1) {
			return empty, binding, errors.New("无法读取推送地址")
		}
		if raw != "" {
			pushURLs := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
			if len(pushURLs) != 1 {
				return empty, binding, errors.New("多个推送地址暂不支持")
			}
			pushURL, e := gitRepositoryURL(pushURLs[0], c)
			if e != nil || pushURL != binding.URL {
				return empty, binding, errors.New("推送地址与绑定地址不同，请使用单独的 remote")
			}
		}
	}
	if err := s.checkGitTerminalScope(ctx, c, binding.URL, write); err != nil {
		return empty, binding, err
	}
	return c, binding, nil
}

func (s *Server) handleGitFetch(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		Repo   string `json:"repo"`
		Remote string `json:"remote"`
	}
	if !decodeGitJSON(w, r, &req) {
		return
	}
	dir, ok := s.gitRepo(w, sess, req.Repo)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 125*time.Second)
	defer cancel()
	unlock, err := s.lockGit(ctx, sess, dir)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	defer unlock()
	c, b, err := s.gitBoundConnection(ctx, sess, dir, req.Remote, false)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, b.Repo, c.ID, "fetch", b.Remote)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	transport, closeFn, err := s.gitTransport(ctx, c, b.URL, false, "", "", "")
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	defer closeFn()
	_, err = s.runGitNetwork(ctx, sess, dir, "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", transport, "+refs/heads/*:refs/remotes/"+b.Remote+"/*")
	if err != nil {
		if ctx.Err() != nil {
			result = "cancelled_unknown"
		}
		writeErr(w, 502, "获取失败：请检查连接权限、Token、网络及仓库地址；本地文件未合并更新")
		return
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"ok": true, "operation_id": op, "fetched_at": time.Now().UTC().Format(time.RFC3339Nano)})
}

type gitPushRequest struct {
	Repo       string `json:"repo"`
	Remote     string `json:"remote"`
	Head       string `json:"expected_head"`
	RemoteHead string `json:"expected_remote_head"`
	Ref        string `json:"ref"`
}

func gitOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func (s *Server) handleGitPushPreview(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.gitPush(w, r, sess, true)
}
func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.gitPush(w, r, sess, false)
}
func (s *Server) gitPush(w http.ResponseWriter, r *http.Request, sess store.Session, preview bool) {
	var req gitPushRequest
	if !decodeGitJSON(w, r, &req) {
		return
	}
	dir, ok := s.gitRepo(w, sess, req.Repo)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 125*time.Second)
	defer cancel()
	unlock, err := s.lockGit(ctx, sess, dir)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	defer unlock()
	c, b, err := s.gitBoundConnection(ctx, sess, dir, req.Remote, true)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	ref, err := s.runGit(ctx, sess, dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		writeErr(w, 409, "游离 HEAD 不能直接推送，请先切换或创建分支")
		return
	}
	ref = strings.TrimSpace(ref)
	head, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", "HEAD")
	head = strings.TrimSpace(head)
	if err != nil || !gitOID(head) || !strings.HasPrefix(ref, "refs/heads/") {
		writeErr(w, 409, "当前分支还没有可推送的提交")
		return
	}
	operation := "push"
	if preview {
		operation = "push-preview"
	}
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, b.Repo, c.ID, operation, b.Remote+":"+ref)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	transport, closeFn, err := s.gitTransport(ctx, c, b.URL, false, "", "", "")
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	remoteOut, err := s.runGitNetwork(ctx, sess, dir, "ls-remote", "--refs", "--", transport, ref)
	closeFn()
	if err != nil {
		writeErr(w, 502, "无法读取远程分支，请检查连接权限、Token 和网络")
		return
	}
	remoteHead := strings.Repeat("0", len(head))
	for _, line := range strings.Split(strings.TrimSpace(remoteOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			if !gitOID(fields[0]) {
				writeErr(w, 502, "远程分支返回无效提交编号")
				return
			}
			remoteHead = fields[0]
		}
	}
	if !preview && (req.Head != head || req.RemoteHead != remoteHead || req.Ref != ref) {
		writeErr(w, 409, "本地或远程分支已变化，请重新预览推送")
		return
	}
	if remoteHead == head {
		writeErr(w, 409, "本地与远程分支已一致，无需推送")
		return
	}
	if strings.Trim(remoteHead, "0") != "" {
		if _, err = s.runGit(ctx, sess, dir, "merge-base", "--is-ancestor", remoteHead, head); err != nil {
			writeErr(w, 409, "远程存在本地未包含的提交，请先获取并完成合并；不支持强制推送")
			return
		}
	}
	if preview {
		args := []string{"log", "-20", "--format=%h %s", head}
		if strings.Trim(remoteHead, "0") != "" {
			args = append(args, "^"+remoteHead)
		}
		args = append(args, "--")
		commits, err := s.runGit(ctx, sess, dir, args...)
		if err != nil {
			writeGitErr(w, err)
			return
		}
		result = "success"
		writeJSON(w, 200, map[string]any{"repo": b.Repo, "remote": b.Remote, "url": b.URL, "connection": c.Label, "ref": ref, "expected_head": head, "expected_remote_head": remoteHead, "commits": strings.TrimSpace(commits), "new_branch": strings.Trim(remoteHead, "0") == ""})
		return
	}
	transport, closeFn, err = s.gitTransport(ctx, c, b.URL, true, ref, remoteHead, head)
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	defer closeFn()
	_, err = s.runGitNetwork(ctx, sess, dir, "-c", "push.followTags=false", "-c", "push.gpgSign=false", "push", "--porcelain", "--no-verify", "--no-follow-tags", "--", transport, head+":"+ref)
	if err != nil {
		result = "failed_unknown"
		writeErr(w, 502, "推送未确认成功：远程可能拒绝了权限、保护分支或并发更新；请重新预览核实远程状态，已有本地提交保留")
		return
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"ok": true, "sha": head, "ref": ref, "pushed": true, "operation_id": op})
}

// Pull is deliberately fast-forward only and requires a clean index/worktree.
// Fetch updates cached refs first; a failed merge leaves commits local.
func (s *Server) handleGitPull(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		Repo   string `json:"repo"`
		Remote string `json:"remote"`
	}
	if !decodeGitJSON(w, r, &req) {
		return
	}
	dir, ok := s.gitRepo(w, sess, req.Repo)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	unlock, err := s.lockGit(ctx, sess, dir)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	defer unlock()
	c, b, err := s.gitBoundConnection(ctx, sess, dir, req.Remote, false)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	status, err := s.runGit(ctx, sess, dir, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		writeGitErr(w, err)
		return
	}
	branch, files := parseGitState(status)
	if len(files) != 0 || branch.Detached || branch.Unborn {
		writeErr(w, 409, "快进拉取需要已提交且干净的本地分支；请先处理工作区改动")
		return
	}
	upstreamRemote, err := s.runGit(ctx, sess, dir, "config", "--local", "--get", "branch."+branch.Branch+".remote")
	if err != nil || strings.TrimSpace(upstreamRemote) != b.Remote {
		writeErr(w, 409, "当前分支的上游不是所选 remote，请先设置正确的上游分支")
		return
	}
	upstreamRef, err := s.runGit(ctx, sess, dir, "config", "--local", "--get", "branch."+branch.Branch+".merge")
	upstreamRef = strings.TrimSpace(upstreamRef)
	if err != nil || !strings.HasPrefix(upstreamRef, "refs/heads/") {
		writeErr(w, 409, "上游分支设置无效")
		return
	}
	if _, err = s.runGit(ctx, sess, dir, "check-ref-format", upstreamRef); err != nil {
		writeErr(w, 409, "上游分支设置无效")
		return
	}
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, b.Repo, c.ID, "pull", b.Remote+":"+upstreamRef)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	transport, closeFn, err := s.gitTransport(ctx, c, b.URL, false, "", "", "")
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	defer closeFn()
	tracking := "refs/remotes/" + b.Remote + "/" + strings.TrimPrefix(upstreamRef, "refs/heads/")
	if _, err = s.runGitNetwork(ctx, sess, dir, "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", transport, "+"+upstreamRef+":"+tracking); err != nil {
		writeErr(w, 502, "获取上游分支失败，工作文件未合并更新")
		return
	}
	gitPhase(ctx, "checking")
	// Recheck before changing the worktree: terminals may have edited during fetch.
	current, err := s.runGit(ctx, sess, dir, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		writeGitErr(w, err)
		return
	}
	now, changed := parseGitState(current)
	if now.Head != branch.Head || now.Branch != branch.Branch || len(changed) != 0 {
		writeErr(w, 409, "获取期间工作区发生变化，请重新检查后拉取")
		return
	}
	target, err := s.runGit(ctx, sess, dir, "rev-parse", "--verify", tracking)
	target = strings.TrimSpace(target)
	if err != nil || !gitOID(target) {
		writeErr(w, 409, "无法读取获取的上游提交")
		return
	}
	if _, err = s.runGit(ctx, sess, dir, "merge-base", "--is-ancestor", branch.Head, target); err != nil {
		writeErr(w, 409, "本地与远程存在分叉或本地领先，无法快进拉取；请在终端处理")
		return
	}
	gitPhase(ctx, "merging")
	if _, err = s.runGit(ctx, sess, dir, "merge", "--ff-only", "--no-edit", target); err != nil {
		writeErr(w, 409, "快进合并失败，请检查终端中的 Git 状态；不会自动变基或覆盖本地提交")
		return
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"ok": true, "sha": target, "operation_id": op})
}

func (s *Server) handleGitClone(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		ConnectionID string `json:"connection_id"`
		URL          string `json:"url"`
		Directory    string `json:"directory"`
	}
	if !decodeGitJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Directory)
	if !gitText(name, 128, false) || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") || !filepath.IsLocal(name) {
		writeErr(w, 400, "请填写空间根目录下的新文件夹名称，不能以点开头或包含路径分隔符")
		return
	}
	c, err := s.store.GitConnectionFor(sess.User, req.ConnectionID)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	if !c.Enabled {
		writeErr(w, 409, "Git 连接已停用")
		return
	}
	repository, err := gitRepositoryURL(req.URL, c)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	root, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	if _, err = root.Lstat(name); err == nil {
		writeErr(w, 409, "目标文件夹已存在，请选择其他名称")
		return
	} else if !os.IsNotExist(err) {
		writeFileOpErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	unlock, err := s.git.Lock(ctx, sess.ID, name)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	defer unlock()
	op, err := s.beginGitOperation(ctx, sess.User, sess.ID, name, c.ID, "clone", repository)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	transport, closeFn, err := s.gitTransport(ctx, c, repository, false, "", "", "")
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	defer closeFn()
	temp := ".abox-clone-" + store.NewID() + store.NewID()
	defer root.RemoveAll(temp)
	_, err = s.runGitNetwork(ctx, sess, s.workspaceDir(sess), "clone", "--no-checkout", "--no-tags", "--no-recurse-submodules", "--", transport, "/workspace/"+temp)
	if err != nil {
		writeErr(w, 502, "克隆失败：请检查连接权限、Token、网络或仓库地址，目标文件夹未发布")
		return
	}
	gitPhase(ctx, "checkout")
	tempDir := filepath.Join(s.workspaceDir(sess), temp)
	// Replace the ephemeral grant URL before exposing the completed repository.
	if _, err = s.runGit(ctx, sess, tempDir, "remote", "set-url", "origin", repository); err != nil {
		writeErr(w, 500, "克隆已传输但远程配置失败，目标文件夹未发布")
		return
	}
	if _, err = s.runGit(ctx, sess, tempDir, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		if _, err = s.runGit(ctx, sess, tempDir, "reset", "--hard", "HEAD"); err != nil {
			writeErr(w, 500, "检出克隆内容失败，目标文件夹未发布")
			return
		}
	} else if !gitx.IsExit(err, 1) {
		writeGitErr(w, err)
		return
	}
	gitPhase(ctx, "publishing")
	if err = root.RenameTo(temp, root, name, false); err != nil {
		writeFileOpErr(w, err)
		return
	}
	warning := ""
	b := store.GitBinding{SessionID: sess.ID, Repo: name, Remote: "origin", URL: repository, ConnectionID: c.ID}
	if err = s.store.SaveGitBinding(sess.User, b, 0); err != nil {
		warning = "仓库已克隆，但连接绑定未保存，请到远程设置重新绑定"
		result = "success_binding_failed"
	} else {
		result = "success"
	}
	writeJSON(w, 201, map[string]any{"repo": name, "ok": true, "warning": warning, "operation_id": op})
}

// Probe a specific repository over smart HTTP; never infer write permission
// from read success (PAT scopes and protected branches are separate controls).
func (s *Server) handleGitConnectionTest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL string `json:"url"`
	}
	if !decodeGitJSON(w, r, &input) {
		return
	}
	c, err := s.store.GitConnectionFor(reqUser(r).Name, r.PathValue("connection"))
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	if !c.Enabled {
		writeErr(w, 409, "连接已停用")
		return
	}
	repository, err := gitRepositoryURL(input.URL, c)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if c.AuthType == "ssh" {
		s.testGitSSH(w, r, c, repository)
		return
	}
	c, secret, err := s.gitCredential(r.Context(), c)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	client, closeClient, err := s.gitClient(ctx, c)
	if err != nil {
		writeErr(w, 503, "Git 网络路由不可用")
		return
	}
	defer closeClient()
	safeClient := *client
	safeClient.Jar = nil
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	op, err := s.beginGitOperation(ctx, gitActor(c), "", "", c.ID, "connection.test", repository)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	req, err := http.NewRequestWithContext(ctx, "GET", repository+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		writeErr(w, 400, "仓库地址无效")
		return
	}
	req.SetBasicAuth(c.Username, secret)
	req.Header.Set("User-Agent", "agentbox-git")
	resp, err := safeClient.Do(req)
	if err != nil {
		writeErr(w, 502, "无法连接 Git 服务，请检查地址、网络和 TLS 证书")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeErr(w, 502, fmt.Sprintf("Git 服务拒绝读取（HTTP %d），请检查 Token 与仓库权限", resp.StatusCode))
		return
	}
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || strings.Split(resp.Header.Get("Content-Type"), ";")[0] != "application/x-git-upload-pack-advertisement" || !strings.Contains(string(prefix), "# service=git-upload-pack\n") {
		writeErr(w, 502, "目标没有返回有效的 Git smart HTTP 仓库信息")
		return
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"read_access": true, "write_access": "untested", "tested_at": time.Now().UTC().Format(time.RFC3339Nano)})
}
