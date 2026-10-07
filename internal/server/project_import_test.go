package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func prepareImportReceipt(t *testing.T, s *Server, sess store.Session) store.User {
	t.Helper()
	u, ok := s.store.GetUser(sess.User)
	if !ok {
		if err := s.store.CreateUser(store.User{Name: sess.User, Role: store.RoleUser}); err != nil {
			t.Fatal(err)
		}
		u, _ = s.store.GetUser(sess.User)
	}
	sess.Agent = config.AgentCodex
	sess.AccountID = "fixture"
	sess.DefaultModel = "fixture"
	sess.Status = store.StatusStopped
	sess.CreatedAt = time.Now()
	s.cfg.Accounts = []config.Account{{ID: "fixture", Type: config.AgentCodex}}
	_, err := s.store.ReserveWorkspaceCreation(u, store.WorkspaceCreation{RequestID: strings.Repeat("a", 32), Fingerprint: "fixture", Request: json.RawMessage(`{"name":"fixture","source":"upload","directory":"project"}`), Session: sess})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.CompleteWorkspaceCreation(u, strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	return u
}
func importRequest(u store.User, method, url string, body *bytes.Buffer) *http.Request {
	r := httptest.NewRequest(method, url, body)
	r.SetPathValue("request", strings.Repeat("a", 32))
	return r.WithContext(context.WithValue(r.Context(), ctxUser, u))
}
func uploadProject(t *testing.T, s *Server, u store.User, attempt, directory, mode string, paths []string, contents [][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("mode", mode)
	raw, _ := json.Marshal(paths)
	_ = writer.WriteField("paths", string(raw))
	for i, data := range contents {
		name := filepath.Base(paths[i])
		if mode == "archive" {
			name = "project.zip"
		}
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(data)
	}
	writer.Close()
	r := importRequest(u, "POST", "/upload?attempt_id="+attempt+"&directory="+directory, &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	s.handleProjectUpload(w, r)
	return w
}

func TestProjectUploadAtomicPublicationAndReplay(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 && os.Geteuid() != 1000 {
		t.Skip("requires container ownership permissions; dedicated denial covered separately")
	}
	s, sess := newTestServer(t)
	u := prepareImportReceipt(t, s, sess)
	id := strings.Repeat("b", 32)
	w := uploadProject(t, s, u, id, "project", "files", []string{"src/main.txt", "README.md"}, [][]byte{[]byte("original"), []byte("readme")})
	var result store.WorkspaceImport
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != "succeeded" {
		t.Fatal(w.Code, w.Body.String())
	}
	file := filepath.Join(s.workspaceDir(sess), "project/src/main.txt")
	if err := os.WriteFile(file, []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	w = uploadProject(t, s, u, id, "project", "files", []string{"src/main.txt", "README.md"}, [][]byte{[]byte("original"), []byte("readme")})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != "user edit" {
		t.Fatal("replay overwrote edited files")
	}
	w = uploadProject(t, s, u, id, "project", "files", []string{"src/main.txt", "README.md"}, [][]byte{[]byte("changed"), []byte("readme")})
	if w.Code != 409 {
		t.Fatal("same ID accepted changed content", w.Body.String())
	}
	w = uploadProject(t, s, u, strings.Repeat("c", 32), "project", "files", []string{"replace.txt"}, [][]byte{[]byte("bad")})
	if w.Code != 409 {
		t.Fatal("existing target was merged")
	}
	if entries, err := os.ReadDir(s.cfg.DataDir); err != nil {
		t.Fatal(err)
	} else {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".stage-project-") {
				t.Fatal("staging leak")
			}
		}
	}
}

func TestProjectUploadArchivePreservesTreeAndReplay(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 && os.Geteuid() != 1000 {
		t.Skip("requires container ownership permissions")
	}
	s, sess := newTestServer(t)
	u := prepareImportReceipt(t, s, sess)
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	for name, content := range map[string]string{"demo/src/main.txt": "source", "demo/.gitignore": "build/"} {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 32)
	for range 2 {
		w := uploadProject(t, s, u, id, "project", "archive", []string{"project.zip"}, [][]byte{archive.Bytes()})
		var result store.WorkspaceImport
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != "succeeded" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for name, want := range map[string]string{"demo/src/main.txt": "source", "demo/.gitignore": "build/"} {
		got, err := os.ReadFile(filepath.Join(s.workspaceDir(sess), "project", name))
		if err != nil || string(got) != want {
			t.Fatalf("archive tree %s: %q %v", name, got, err)
		}
	}
	items, err := s.store.WorkspaceImports(u, strings.Repeat("a", 32))
	if err != nil || len(items) != 1 {
		t.Fatalf("archive replay: %+v %v", items, err)
	}
}

func TestProjectUploadRejectsArchivesAndPathsWithoutPublishing(t *testing.T) {
	s, sess := newTestServer(t)
	u := prepareImportReceipt(t, s, sess)
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	f, _ := z.Create("../outside.txt")
	f.Write([]byte("private"))
	z.Close()
	w := uploadProject(t, s, u, strings.Repeat("b", 32), "project", "archive", []string{"project.zip"}, [][]byte{archive.Bytes()})
	if w.Code < 400 {
		t.Fatal("unsafe archive accepted")
	}
	w = uploadProject(t, s, u, strings.Repeat("c", 32), "project", "files", []string{"../outside.txt"}, [][]byte{[]byte("bad")})
	if w.Code != 400 {
		t.Fatal("unsafe file path accepted")
	}
	if entries, err := os.ReadDir(s.workspaceDir(sess)); err != nil || len(entries) != 0 {
		t.Fatal("failed validation published files")
	}
	items, err := s.store.WorkspaceImports(u, strings.Repeat("a", 32))
	if err != nil || len(items) != 0 {
		t.Fatal("invalid body began a publication")
	}
	s.cfg.MaxUploadMB = 1
	w = uploadProject(t, s, u, strings.Repeat("d", 32), "project", "files", []string{"one", "two"}, [][]byte{bytes.Repeat([]byte("a"), 600<<10), bytes.Repeat([]byte("b"), 600<<10)})
	if w.Code != 413 {
		t.Fatal("aggregate file limit was not enforced", w.Code, w.Body.String())
	}
	if entries, err := os.ReadDir(s.workspaceDir(sess)); err != nil || len(entries) != 0 {
		t.Fatal("oversize request published files")
	}
}

func TestProjectUploadLinuxChownDenied(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ownership semantics")
	}
	if os.Geteuid() == 0 || os.Geteuid() == 1000 {
		if os.Getenv("AGENTBOX_CHOWN_DENIAL_TEST") == "1" {
			t.Fatal("requires non-root user unable to chown to 1000")
		}
		t.Skip("requires unprivileged UID other than 1000")
	}
	s, sess := newTestServer(t)
	u := prepareImportReceipt(t, s, sess)
	w := uploadProject(t, s, u, strings.Repeat("b", 32), "project", "files", []string{"hello.txt"}, [][]byte{[]byte("must not publish")})
	if w.Code != 500 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertProblem(t, w.Body.Bytes(), "storage_permission_denied")
	entries, err := os.ReadDir(s.workspaceDir(sess))
	if err != nil || len(entries) != 0 {
		t.Fatal("chown rejection published project")
	}
	items, err := s.store.WorkspaceImports(u, strings.Repeat("a", 32))
	if err != nil || len(items) != 0 {
		t.Fatal("chown rejection started publication receipt")
	}
}

func TestCreationHTTPConcurrentRetryKeepsOneWorkspace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Linux root acceptance exercises UID 1000 directory creation")
	}
	s, sess := accessTestServer(t)
	u, _ := s.store.GetUser(sess.User)
	id := strings.Repeat("d", 32)
	body := `{"name":"首个项目","agent":"claude","account_id":"shared","git_connection_id":"","source":"empty"}`
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := accessRequest(u.Name, "PUT", "/session-creations/"+id, body)
			r = r.WithContext(context.WithValue(r.Context(), ctxUser, u))
			r.SetPathValue("request", id)
			w := httptest.NewRecorder()
			s.handleCreateSession(w, r)
			if w.Code != 200 && w.Code != 201 {
				t.Error(w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if len(s.store.All()) != 2 {
		t.Fatal("request replay created multiple spaces")
	}
	c, err := s.store.WorkspaceCreation(u, id)
	if err != nil || c.State != "ready" {
		t.Fatal(err)
	}
	if err := s.workspaces().Delete(t.Context(), c.Session.ID, true); err != nil {
		t.Fatal(err)
	}
	r := accessRequest(u.Name, "PUT", "/create", body).WithContext(context.WithValue(t.Context(), ctxUser, u))
	r.SetPathValue("request", id)
	w := httptest.NewRecorder()
	s.handleCreateSession(w, r)
	if w.Code != 410 {
		t.Fatal("deleted workspace resurrected", w.Code, w.Body.String())
	}
}

func TestProjectGitAuthFailureRetryAndReceipt(t *testing.T) {
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
	var denied atomic.Bool
	denied.Store(true)
	var calls atomic.Int32
	cgiHandler := &cgi.Handler{Path: backend, Root: "/", Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "REMOTE_USER=fixture"}}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if denied.Load() {
			w.WriteHeader(401)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	s.gitHTTPClient = func(context.Context, store.GitConnection) (*http.Client, func(), error) {
		return upstream.Client(), func() {}, nil
	}
	connection := createGitConnection(t, s, sess.User, upstream.URL, false)
	u := prepareImportReceipt(t, s, sess)
	clone := func(attempt, url string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]string{"attempt_id": attempt, "connection_id": connection.ID, "url": url, "directory": "project"})
		r := importRequest(u, "POST", "/git", bytes.NewBuffer(raw))
		w := httptest.NewRecorder()
		s.handleProjectGit(w, r)
		return w
	}
	one, two := strings.Repeat("b", 32), strings.Repeat("c", 32)
	w := clone(one, upstream.URL+"/repo.git")
	var result store.WorkspaceImport
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != "failed" {
		t.Fatal(w.Code, w.Body.String())
	}
	count := calls.Load()
	w = clone(one, upstream.URL+"/repo.git")
	if w.Code != 200 || calls.Load() != count {
		t.Fatal("failed attempt was silently replayed")
	}
	denied.Store(false)
	w = clone(two, upstream.URL+"/repo.git")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != "succeeded" {
		t.Fatal(w.Code, w.Body.String())
	}
	count = calls.Load()
	w = clone(two, upstream.URL+"/repo.git")
	if w.Code != 200 || calls.Load() != count {
		t.Fatal("successful clone was repeated")
	}
	w = clone(two, upstream.URL+"/different.git")
	if w.Code != 409 {
		t.Fatal("changed clone request reused receipt")
	}
	if _, err := os.Stat(filepath.Join(s.workspaceDir(sess), "project/.git")); err != nil {
		t.Fatal(err)
	}
}

func TestCreationSummaryKeepsSavedModelAndCurrentPublicLimits(t *testing.T) {
	s, sess := newTestServer(t)
	u := prepareImportReceipt(t, s, sess)
	s.cfg.DefaultModels = map[string]string{config.AgentCodex: "changed-default"}
	s.cfg.Container = config.ContainerLimits{CPUs: 3, MemoryMB: 3072, PidsLimit: 512, Network: "private-network"}
	r := importRequest(u, "GET", "/creation", &bytes.Buffer{})
	w := httptest.NewRecorder()
	s.handleCreationGet(w, r)
	var view creationView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if view.Session.DefaultModel != "fixture" || view.ContainerResources.CPUs != 3 || view.ContainerResources.MemoryMB != 3072 || view.ContainerResources.PidsLimit != 512 {
		t.Fatal("summary confused saved workspace config with current defaults", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-network") || strings.Contains(w.Body.String(), "changed-default") {
		t.Fatal("summary leaked unselected configuration", w.Body.String())
	}
}

func TestCreationReceiptsRequireCurrentOwnerIdentity(t *testing.T) {
	s, sess := accessTestServer(t)
	u, _ := s.store.GetUser(sess.User)
	id := strings.Repeat("e", 32)
	sess.ID = "pending-space"
	if _, err := s.store.ReserveWorkspaceCreation(u, store.WorkspaceCreation{RequestID: id, Fingerprint: "fixture", Request: json.RawMessage(`{"name":"private-project"}`), Session: sess}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob", "root"} {
		if err := s.store.CreateToken("creation-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	for _, user := range []string{"alice", "bob", "root", "unknown"} {
		r := httptest.NewRequest("GET", "/api/session-creations/"+id, nil)
		r.Header.Set("Authorization", "Bearer creation-"+user)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 404
		if user == "alice" {
			want = 200
		}
		if user == "unknown" {
			want = 401
		}
		if w.Code != want {
			t.Fatal(user, w.Code, w.Body.String())
		}
		if user != "alice" && strings.Contains(w.Body.String(), "private-project") {
			t.Fatal("receipt leaked across owner boundary")
		}
	}
}
