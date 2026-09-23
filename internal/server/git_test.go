package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/gitx"
	"agentbox/internal/store"
)

// This executor maps container paths to a temporary fixture for handler tests.
// Production has no local executor; Docker API behavior is tested in dockerx.
type localGitExecutor struct{ workspace string }

func (e localGitExecutor) ExecCommand(ctx context.Context, containerID string, argv []string) (string, error) {
	if containerID != "test-container" {
		return "", errors.New("unexpected container")
	}
	// The fixture uses host Git, so omit the Linux-only timeout wrapper.
	args := append([]string(nil), argv[4:]...)
	for i, arg := range args {
		if arg == "/workspace" || strings.HasPrefix(arg, "/workspace/") {
			args[i] = e.workspace + strings.TrimPrefix(arg, "/workspace")
		} else if strings.HasPrefix(arg, "GIT_CEILING_DIRECTORIES=") {
			dir := strings.TrimPrefix(arg, "GIT_CEILING_DIRECTORIES=")
			if dir == "/" {
				dir = filepath.Dir(e.workspace)
			} else {
				dir = e.workspace + strings.TrimPrefix(dir, "/workspace")
			}
			args[i] = "GIT_CEILING_DIRECTORIES=" + dir
		}
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	returnBytes, err := cmd.Output()
	return string(returnBytes), err
}

func newGitTestServer(t *testing.T) (*Server, store.Session) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, sess := newTestServer(t)
	s.git = gitx.New(localGitExecutor{s.workspaceDir(sess)}, func(context.Context, string) (string, func(), error) {
		return "test-container", func() {}, nil
	})
	return s, sess
}

func TestGitWebCommitDoesNotRunHooks(t *testing.T) {
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A hook installed by the user must not execute when committing in the UI.
	hooks := filepath.Join(ws, "custom-hooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\ntouch hook-ran\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws, "config", "core.hooksPath", hooks)
	w := httptest.NewRecorder()
	s.handleGitCommit(w, httptest.NewRequest(http.MethodPost, "/g/commit", strings.NewReader(`{"message":"first commit"}`)), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "hook-ran")); !os.IsNotExist(err) {
		t.Fatalf("repository hook ran: %v", err)
	}
}

func TestGitDiffBeforeFirstCommitAndUnavailableRuntime(t *testing.T) {
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "first.txt"), []byte("first content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws, "add", "first.txt")
	w := httptest.NewRecorder()
	s.handleGitDiff(w, httptest.NewRequest(http.MethodGet, "/g/diff", nil), sess)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "+first content") {
		t.Fatalf("unborn HEAD diff: %d %s", w.Code, w.Body.String())
	}
	// A runtime failure must not silently become an empty successful diff, and
	// must never invoke host Git as a fallback even though the repo is present.
	s.git = nil
	w = httptest.NewRecorder()
	s.handleGitDiff(w, httptest.NewRequest(http.MethodGet, "/g/diff", nil), sess)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable: %d %s", w.Code, w.Body.String())
	}
}

func TestGitQuotaBlocksBeforeContainerStart(t *testing.T) {
	s, sess := newTestServer(t)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.SetQuotaEnforced(sess.User, true); err != nil {
		t.Fatal(err)
	}
	// idle and dock are nil: reaching either would panic. Rejection must happen
	// before any activity hold, credentials synchronization or Docker operation.
	_, release, err := s.prepareGitSession(t.Context(), sess.ID)
	if !errors.Is(err, errGitQuota) || release != nil {
		t.Fatalf("quota: release present=%v err=%v", release != nil, err)
	}
}

func TestGitDiscardUsesLiteralPathsAndStopsOnRestoreFailure(t *testing.T) {
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q")
	for _, name := range []string{"tracked.txt", "[ab].txt", "a.txt"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, ws, "add", "tracked.txt")
	gitCmd(t, ws, "commit", "-q", "-m", "init")
	w := httptest.NewRecorder()
	s.handleGitDiscard(w, httptest.NewRequest(http.MethodPost, "/g/discard", strings.NewReader(`{"path":"[ab].txt"}`)), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("discard: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "[ab].txt")); !os.IsNotExist(err) {
		t.Fatalf("selected path remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
		t.Fatalf("literal path matched a different file: %v", err)
	}
	// A corrupt HEAD prevents checkout. The untracked file must survive the
	// failed restore rather than being deleted by a subsequent clean.
	if err := os.WriteFile(filepath.Join(ws, ".git", "HEAD"), []byte("ref: refs/heads/missing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleGitDiscard(w, httptest.NewRequest(http.MethodPost, "/g/discard", strings.NewReader(`{}`)), sess)
	if w.Code == http.StatusOK {
		t.Fatal("failed restore reported success")
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
		t.Fatalf("clean ran after failed checkout: %v", err)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func getStatus(t *testing.T, s *Server, sess store.Session, repo string) gitStatusResp {
	t.Helper()
	w := httptest.NewRecorder()
	url := "/g"
	if repo != "" {
		url += "?repo=" + repo
	}
	s.handleGitStatus(w, httptest.NewRequest(http.MethodGet, url, nil), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d: %s", w.Code, w.Body.String())
	}
	var out gitStatusResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type gitStatusResp struct {
	IsRepo bool      `json:"is_repo"`
	Repo   string    `json:"repo"`
	Repos  []string  `json:"repos"`
	Branch string    `json:"branch"`
	Files  []gitFile `json:"files"`
}

// A session workspace lives under data_dir, which in production sits inside the
// server's own checkout. Git's repo discovery must not walk up into it: doing so
// made every session report the server repo's changes, and commit would have
// staged that repo's entire tree.
func TestGitStatusDoesNotEscapeWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, sess := newGitTestServer(t)
	outer := s.cfg.DataDir // encloses the session workspace
	gitCmd(t, outer, "init", "-q")
	if err := os.WriteFile(filepath.Join(outer, "outer.txt"), []byte("host repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, outer, "add", "-A")
	gitCmd(t, outer, "commit", "-q", "-m", "init")
	_ = os.WriteFile(filepath.Join(outer, "outer.txt"), []byte("dirty\n"), 0o644)

	if st := getStatus(t, s, sess, ""); st.IsRepo || len(st.Repos) != 0 {
		t.Fatalf("status = %+v, want no repo (must not see the enclosing repo)", st)
	}
	// Second layer: even asked to run there directly, git must not discover upward.
	if out, err := s.runGit(t.Context(), sess, s.workspaceDir(sess), "rev-parse", "--show-toplevel"); err == nil {
		t.Fatalf("git found a repo at %q, want discovery stopped at the workspace", strings.TrimSpace(out))
	}
	// Write paths refuse to run at all rather than falling back to the enclosing repo.
	w := httptest.NewRecorder()
	s.handleGitCommit(w, httptest.NewRequest(http.MethodPost, "/g/commit", strings.NewReader(`{"message":"x"}`)), sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("commit code = %d, want 400: %s", w.Code, w.Body.String())
	}
	if out, _ := exec.Command("git", "-C", outer, "status", "--porcelain").Output(); !strings.Contains(string(out), "outer.txt") {
		t.Fatalf("enclosing repo was committed into; status:\n%s", out)
	}
}

// An untracked directory must be listed file by file: git's default porcelain
// collapses it to one "dir/" entry, which cannot be diffed or discarded on its
// own. The host's global git config must not filter the list either — the repo
// belongs to the container user, and root's ~/.config/git/ignore (which Claude
// Code seeds with .claude/settings.local.json) would silently hide files.
func TestGitStatusListsUntrackedFilesIndividually(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "tracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws, "add", "-A")
	gitCmd(t, ws, "commit", "-q", "-m", "init")

	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.local.json", "other.json"} {
		if err := os.WriteFile(filepath.Join(ws, ".claude", name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A global ignore that would hide one of them if the server read it.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "git", "ignore"),
		[]byte("**/.claude/settings.local.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	st := getStatus(t, s, sess, "")
	got := map[string]bool{}
	for _, f := range st.Files {
		got[f.Path] = true
	}
	for _, want := range []string{".claude/settings.local.json", ".claude/other.json"} {
		if !got[want] {
			t.Fatalf("files = %+v, want %s listed individually", st.Files, want)
		}
	}
	if got[".claude/"] {
		t.Fatalf("files = %+v, want no collapsed directory entry", st.Files)
	}
}

// Workspaces are rarely repos themselves — the project is normally cloned or
// unzipped into a subdirectory, which is what review should act on.
func TestGitNestedRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	for _, name := range []string{"proj", "later", filepath.Join("node_modules", "dep"), filepath.Join("__MACOSX", "proj")} {
		dir := filepath.Join(ws, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "init", "-q")
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-q", "-m", "init")
		_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed in "+name+"\n"), 0o644)
	}

	st := getStatus(t, s, sess, "")
	if want := []string{"later", "proj"}; len(st.Repos) != 2 || st.Repos[0] != want[0] || st.Repos[1] != want[1] {
		t.Fatalf("repos = %v, want %v (node_modules/__MACOSX skipped)", st.Repos, want)
	}
	if !st.IsRepo || st.Repo != "later" || len(st.Files) != 1 || st.Files[0].Path != "a.txt" {
		t.Fatalf("status = %+v, want the first repo's own changes", st)
	}
	// Selecting a repo switches which one is reviewed; paths stay repo-relative.
	if st := getStatus(t, s, sess, "proj"); st.Repo != "proj" || len(st.Files) != 1 {
		t.Fatalf("status(proj) = %+v, want proj with 1 change", st)
	}
	w := httptest.NewRecorder()
	s.handleGitDiff(w, httptest.NewRequest(http.MethodGet, "/g/diff?repo=proj", nil), sess)
	if !strings.Contains(w.Body.String(), "changed in proj") {
		t.Fatalf("diff did not target proj:\n%s", w.Body.String())
	}
	// Committing one repo leaves the other alone.
	w = httptest.NewRecorder()
	s.handleGitCommit(w, httptest.NewRequest(http.MethodPost, "/g/commit", strings.NewReader(`{"message":"m","repo":"proj"}`)), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("commit code = %d: %s", w.Code, w.Body.String())
	}
	if st := getStatus(t, s, sess, "proj"); len(st.Files) != 0 {
		t.Fatalf("proj files = %+v, want clean", st.Files)
	}
	if st := getStatus(t, s, sess, "later"); len(st.Files) != 1 {
		t.Fatalf("later files = %+v, want its change untouched", st.Files)
	}
	// An unknown repo is rejected outright, never redirected to another one.
	w = httptest.NewRecorder()
	s.handleGitDiscard(w, httptest.NewRequest(http.MethodPost, "/g/discard", strings.NewReader(`{"repo":"../.."}`)), sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("discard with bogus repo code = %d, want 400", w.Code)
	}
}

// Review has to be able to read a file, not just its diff: a new file has no
// diff against HEAD at all. Content comes from the working tree, is repo- (not
// workspace-) relative, and must stay inside the repo.
func TestGitFileView(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	proj := filepath.Join(ws, "proj")
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(proj, rel), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, proj, "init", "-q")
	write("tracked.txt", []byte("one\n"))
	write("gone.txt", []byte("bye\n"))
	gitCmd(t, proj, "add", "-A")
	gitCmd(t, proj, "commit", "-q", "-m", "init")
	write("tracked.txt", []byte("one\ntwo\n"))
	write(".claude/settings.local.json", []byte("{\"new\":true}\n"))
	write("blob.bin", []byte{0x1f, 0x00, 0x8b, 0x02})
	write("big.log", make([]byte, maxFileViewBytes+1))
	if err := os.Remove(filepath.Join(proj, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	// Outside the repo but inside the workspace: still off limits.
	if err := os.WriteFile(filepath.Join(ws, "outside.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ws, filepath.Join(proj, "up")); err != nil {
		t.Fatal(err)
	}

	get := func(p string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		url := "/g/file?repo=proj&path=" + url.QueryEscape(p)
		s.handleGitFile(w, httptest.NewRequest(http.MethodGet, url, nil), sess)
		return w
	}
	// A new file: content, even though `git diff HEAD` says nothing about it.
	if w := get(".claude/settings.local.json"); w.Code != http.StatusOK || w.Body.String() != "{\"new\":true}\n" {
		t.Fatalf("new file = %d %q, want its content", w.Code, w.Body.String())
	}
	// A modified file shows the working tree, not HEAD.
	if w := get("tracked.txt"); w.Code != http.StatusOK || w.Body.String() != "one\ntwo\n" {
		t.Fatalf("tracked.txt = %d %q, want the working-tree version", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		name, path string
		code       int
	}{
		{"deleted", "gone.txt", http.StatusNotFound},
		{"binary", "blob.bin", http.StatusBadRequest},
		{"too big", "big.log", http.StatusBadRequest},
		{"escape", "../outside.txt", http.StatusBadRequest},
		{"symlink", "up/outside.txt", http.StatusBadRequest},
	} {
		if w := get(tc.path); w.Code != tc.code {
			t.Fatalf("%s: code = %d, want %d: %s", tc.name, w.Code, tc.code, w.Body.String())
		} else if strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%s: leaked content outside the repo: %s", tc.name, w.Body.String())
		}
	}
}

func TestGitStatusDiffCommitDiscard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)

	git := func(args ...string) { gitCmd(t, ws, args...) }
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	// Modify a tracked file and add an untracked one.
	_ = os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello world\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "b.txt"), []byte("new file\n"), 0o644)

	st := getStatus(t, s, sess, "")
	if !st.IsRepo || st.Repo != "" || len(st.Files) != 2 {
		t.Fatalf("status = %+v, want the workspace root itself with 2 changed files", st)
	}

	// diff shows the tracked change.
	w := httptest.NewRecorder()
	s.handleGitDiff(w, httptest.NewRequest(http.MethodGet, "/g/diff", nil), sess)
	if !strings.Contains(w.Body.String(), "hello world") {
		t.Fatalf("diff missing change:\n%s", w.Body.String())
	}

	// commit clears the working tree.
	w = httptest.NewRecorder()
	s.handleGitCommit(w, httptest.NewRequest(http.MethodPost, "/g/commit", strings.NewReader(`{"message":"change"}`)), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("commit code = %d: %s", w.Code, w.Body.String())
	}
	if st := getStatus(t, s, sess, ""); len(st.Files) != 0 {
		t.Fatalf("after commit files = %+v, want clean", st.Files)
	}

	// discard restores a subsequent edit.
	_ = os.WriteFile(filepath.Join(ws, "a.txt"), []byte("dirty\n"), 0o644)
	w = httptest.NewRecorder()
	s.handleGitDiscard(w, httptest.NewRequest(http.MethodPost, "/g/discard", strings.NewReader(`{}`)), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("discard code = %d: %s", w.Code, w.Body.String())
	}
	if raw, _ := os.ReadFile(filepath.Join(ws, "a.txt")); string(raw) != "hello world\n" {
		t.Fatalf("discard did not restore a.txt: %q", raw)
	}

	// empty commit message is rejected.
	w = httptest.NewRecorder()
	s.handleGitCommit(w, httptest.NewRequest(http.MethodPost, "/g/commit", strings.NewReader(`{"message":"  "}`)), sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty message commit code = %d, want 400", w.Code)
	}
}
