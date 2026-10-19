package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

type gitOperationPoll struct {
	State     string           `json:"state"`
	Status    int              `json:"status"`
	Response  json.RawMessage  `json:"response"`
	Operation gitOperationView `json:"operation"`
}

func pollGitOperation(t *testing.T, s *Server, user, id string) (int, gitOperationPoll) {
	t.Helper()
	r := accessRequest(user, "GET", "/git/operations/"+id, "")
	r.SetPathValue("operation", id)
	w := httptest.NewRecorder()
	s.handleGitOperation(w, r)
	var poll gitOperationPoll
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &poll); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, poll
}

func waitGitOperation(t *testing.T, s *Server, user, id string) gitOperationPoll {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, poll := pollGitOperation(t, s, user, id)
		if code != 200 {
			t.Fatalf("poll: %d", code)
		}
		if poll.State == "done" {
			return poll
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return gitOperationPoll{}
}

func TestGitAsyncOperationKeepsResponseAndOwner(t *testing.T) {
	s, _ := newTestServer(t)
	release := make(chan struct{})
	handler := s.gitOperation("fetch", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		<-release
		if r.Context().Err() != nil {
			t.Error("background context ended with the request")
		}
		gitPhase(r.Context(), "transferring")
		writeErr(w, gitUpstreamStatus, "upstream said no: "+string(raw)+" "+r.PathValue("id"))
	}))
	id := strings.Repeat("a", 32)
	request := func(id string) *httptest.ResponseRecorder {
		r := accessRequest("alice", "POST", "/sessions/s1/git/fetch", `{"repo":"x"}`)
		r.SetPathValue("id", "s1")
		r.Header.Set("X-Git-Request-ID", id)
		r.Header.Set("X-Git-Async", "1")
		ctx, cancel := context.WithCancel(r.Context())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		cancel() // The HTTP request ends long before the transfer.
		return w
	}
	if w := request(id); w.Code != 202 || !strings.Contains(w.Body.String(), `"async":true`) {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if code, poll := pollGitOperation(t, s, "alice", id); code != 200 || poll.State != "running" || poll.Operation.Operation != "fetch" {
		t.Fatalf("running: %d %+v", code, poll)
	}
	if code, _ := pollGitOperation(t, s, "bob", id); code != 404 {
		t.Fatalf("other user saw operation: %d", code)
	}
	close(release)
	poll := waitGitOperation(t, s, "alice", id)
	var body map[string]string
	if poll.Status != gitUpstreamStatus || json.Unmarshal(poll.Response, &body) != nil || body["error"] != `upstream said no: {"repo":"x"} s1` {
		t.Fatalf("result: %+v %s", poll, poll.Response)
	}
	// A finished ID cannot start the operation a second time.
	if w := request(id); w.Code != 409 {
		t.Fatalf("replayed finished operation: %d", w.Code)
	}
	if code, poll := pollGitOperation(t, s, "alice", id); code != 200 || poll.State != "done" {
		t.Fatal("result not kept for later polls")
	}
}

func TestGitAsyncOperationCancel(t *testing.T) {
	s, _ := newTestServer(t)
	handler := s.gitOperation("clone", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		writeErr(w, gitUpstreamStatus, "cancelled")
	}))
	id := strings.Repeat("b", 32)
	r := accessRequest("alice", "POST", "/git/clone", `{}`)
	r.Header.Set("X-Git-Request-ID", id)
	r.Header.Set("X-Git-Async", "1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	cancel := accessRequest("alice", "POST", "/git/operations/"+id+"/cancel", "")
	cancel.SetPathValue("operation", id)
	cw := httptest.NewRecorder()
	s.handleGitOperationCancel(cw, cancel)
	if cw.Code != 202 {
		t.Fatal(cw.Code)
	}
	if poll := waitGitOperation(t, s, "alice", id); poll.Status != gitUpstreamStatus || !poll.Operation.CancelRequested {
		t.Fatalf("%+v", poll)
	}
}

func TestGitLabRepositoryURLAddsGitSuffix(t *testing.T) {
	gitlab := store.GitConnection{Provider: "gitlab", AuthType: "pat", BaseURL: "http://git.lan/gitlab"}
	for raw, want := range map[string]string{
		"http://git.lan/gitlab/team/sub/project":     "http://git.lan/gitlab/team/sub/project.git",
		"http://git.lan/gitlab/team/sub/project/":    "http://git.lan/gitlab/team/sub/project.git",
		"http://git.lan/gitlab/team/sub/project.git": "http://git.lan/gitlab/team/sub/project.git",
	} {
		if got, err := gitRepositoryURL(raw, gitlab); err != nil || got != want {
			t.Errorf("%s: %q %v", raw, got, err)
		}
	}
	generic := store.GitConnection{Provider: "generic", AuthType: "pat", BaseURL: "https://git.example.com"}
	if got, err := gitRepositoryURL("https://git.example.com/team/project", generic); err != nil || got != "https://git.example.com/team/project" {
		t.Fatalf("generic changed: %q %v", got, err)
	}
}

func TestProjectGitAsyncImportReportsRunningThenResult(t *testing.T) {
	s, sess := newGitTestServer(t)
	s.cfg.ProxyBridge = config.ProxyBridgeConfig{Bind: "127.0.0.1:0", Host: "127.0.0.1"}
	root := t.TempDir()
	remote := filepath.Join(root, "repo.git")
	if err := os.Mkdir(remote, 0755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, remote, "init", "--bare", "-q", "-b", "main")
	gitPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(gitPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend unavailable")
	}
	gate := make(chan struct{})
	cgiHandler := &cgi.Handler{Path: backend, Root: "/", Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "REMOTE_USER=fixture"}}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		cgiHandler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	s.gitHTTPClient = func(context.Context, store.GitConnection) (*http.Client, func(), error) {
		return upstream.Client(), func() {}, nil
	}
	connection := createGitConnection(t, s, sess.User, upstream.URL, false)
	u := prepareImportReceipt(t, s, sess)
	attempt := strings.Repeat("d", 32)
	raw, _ := json.Marshal(map[string]string{"attempt_id": attempt, "connection_id": connection.ID, "url": upstream.URL + "/repo", "directory": "project"})
	r := importRequest(u, "POST", "/git", bytes.NewBuffer(raw))
	r.Header.Set("X-Git-Async", "1")
	w := httptest.NewRecorder()
	s.handleProjectGit(w, r)
	var started store.WorkspaceImport
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &started) != nil || started.State != "running" {
		t.Fatal(w.Code, w.Body.String())
	}
	if !s.creationBusy(sess.ID) {
		t.Fatal("creation not busy while the import runs")
	}
	// Progress uses the attempt ID once the background job has registered it.
	running := false
	for deadline := time.Now().Add(5 * time.Second); !running && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		code, poll := pollGitOperation(t, s, u.Name, attempt)
		running = code == 200 && poll.State == "running" && poll.Operation.Operation == "clone"
	}
	if !running {
		t.Fatal("clone progress not reported")
	}
	close(gate)
	deadline := time.Now().Add(20 * time.Second)
	for s.creationBusy(sess.ID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	imports, err := s.store.WorkspaceImports(u, strings.Repeat("a", 32))
	if err != nil || len(imports) != 1 || imports[0].State != "succeeded" {
		t.Fatalf("receipt: %+v %v", imports, err)
	}
	if _, err := os.Stat(filepath.Join(s.workspaceDir(sess), "project/.git")); err != nil {
		t.Fatal(err)
	}
	// The GitLab connection fixture records the .git URL GitLab redirects to.
	bindings, err := s.store.GitBindings(sess.ID)
	if err != nil || len(bindings) != 1 || bindings[0].URL != upstream.URL+"/repo.git" {
		t.Fatalf("binding: %+v %v", bindings, err)
	}
}
