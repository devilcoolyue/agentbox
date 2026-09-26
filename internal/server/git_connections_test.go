package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func createGitConnection(t *testing.T, s *Server, owner, base string, readOnly bool) store.GitConnection {
	t.Helper()
	if _, ok := s.store.GetUser(owner); !ok {
		if err := s.store.CreateUser(store.User{Name: owner, Role: store.RoleUser, PassHash: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := json.Marshal(map[string]any{"label": "Fixture Git", "provider": "gitlab", "base_url": base, "username": "fixture", "token": "synthetic-secret", "read_only": readOnly})
	w := httptest.NewRecorder()
	s.handleGitConnections(w, accessRequest(owner, "POST", "/git/connections", string(body)))
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "synthetic-secret") || strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatal("secret returned by API")
	}
	var c store.GitConnection
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestGitConnectionsIsolationRevisionsAndRevocation(t *testing.T) {
	s, sess := newTestServer(t)
	c := createGitConnection(t, s, sess.User, "https://git.example.com", true)
	stored, err := s.store.GitConnection(sess.User, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Secret, []byte("synthetic-secret")) {
		t.Fatal("plaintext in database")
	}
	plaintext, err := s.gitVault().Open(stored.Secret, stored.AssociatedData())
	if err != nil || string(plaintext) != "synthetic-secret" {
		t.Fatalf("decrypt: %v", err)
	}
	for _, user := range []string{"bob", "root"} {
		w := httptest.NewRecorder()
		r := accessRequest(user, "PATCH", "/git/connections/"+c.ID, `{"revision":1,"enabled":false}`)
		r.SetPathValue("connection", c.ID)
		s.handleGitConnection(w, r)
		if w.Code != 404 {
			t.Fatalf("%s changed someone else's credential: %d", user, w.Code)
		}
		w = httptest.NewRecorder()
		s.handleGitConnections(w, accessRequest(user, "GET", "/git/connections", ""))
		if w.Code != 200 || w.Body.String() != "[]\n" {
			t.Fatalf("list isolation: %s", w.Body.String())
		}
	}
	patch := func(payload string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := accessRequest(sess.User, "PATCH", "/git/connections/"+c.ID, payload)
		r.SetPathValue("connection", c.ID)
		s.handleGitConnection(w, r)
		return w
	}
	if w := patch(`{"revision":1,"enabled":false}`); w.Code != 200 {
		t.Fatalf("disable: %s", w.Body.String())
	}
	if w := patch(`{"revision":1,"label":"stale"}`); w.Code != 409 {
		t.Fatalf("stale overwrite: %d", w.Code)
	}
	if w := patch(`{"revision":2,"base_url":"https://evil.example.com"}`); w.Code != 400 {
		t.Fatalf("endpoint changed: %d", w.Code)
	}
	if err := s.store.SetGitDefault(sess.User, "", c.ID); err == nil {
		t.Fatal("disabled default accepted")
	}
	if w := patch(`{"revision":2,"enabled":true}`); w.Code != 200 {
		t.Fatalf("enable: %s", w.Body.String())
	}
	if err := s.store.SetGitDefault(sess.User, "", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.store.DeleteGitConnection(sess.User, c.ID, 3); err != store.ErrGitInUse {
		t.Fatalf("deleted default: %v", err)
	}
	if err := s.store.SetGitDefault(sess.User, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.store.DeleteGitConnection(sess.User, c.ID, 3); err != nil {
		t.Fatal(err)
	}
}

// Real Git smart HTTP with synthetic credentials and bare repos. This exercises
// the HTTP protocol and production gitx argv through the local test executor;
// it does not claim Docker/network namespace coverage.
func TestGitHTTPSFetchAndExplicitPush(t *testing.T) {
	s, sess := newGitTestServer(t)

	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.cfg.ProxyBridge = config.ProxyBridgeConfig{Bind: "127.0.0.1:0", Host: "127.0.0.1"}
	root := t.TempDir()
	remote := filepath.Join(root, "repo.git")
	if err := os.Mkdir(remote, 0755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, remote, "init", "--bare", "-q", "-b", "main")
	gitCmd(t, remote, "config", "http.receivepack", "true")
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err = os.Stat(backend); err != nil {
		t.Skip("git-http-backend not available")
	}
	cgiHandler := &cgi.Handler{Path: backend, Root: "/", Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "REMOTE_USER=fixture"}}
	authenticated := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "fixture" || p != "synthetic-secret" {
			t.Error("wrong upstream credential")
			w.WriteHeader(401)
			return
		}
		authenticated++
		cgiHandler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	s.gitHTTPClient = func(context.Context, store.GitConnection) (*http.Client, func(), error) {
		return upstream.Client(), func() {}, nil
	}
	c := createGitConnection(t, s, sess.User, upstream.URL, false)
	probeBody, _ := json.Marshal(map[string]string{"url": upstream.URL + "/repo.git"})
	probeRequest := accessRequest(sess.User, "POST", "/git/connections/"+c.ID+"/test", string(probeBody))
	probeRequest.SetPathValue("connection", c.ID)
	probeResponse := httptest.NewRecorder()
	s.handleGitConnectionTest(probeResponse, probeRequest)
	if probeResponse.Code != 200 || !strings.Contains(probeResponse.Body.String(), `"write_access":"untested"`) {
		t.Fatalf("probe: %d %s", probeResponse.Code, probeResponse.Body.String())
	}

	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q", "-b", "main")
	gitCmd(t, ws, "remote", "add", "origin", upstream.URL+"/repo.git")
	if err = os.WriteFile(filepath.Join(ws, "test.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws, "add", "-A")
	gitCmd(t, ws, "commit", "-q", "-m", "first commit")
	binding, _ := json.Marshal(map[string]any{"repo": "", "remote": "origin", "connection_id": c.ID, "revision": 0})
	w := httptest.NewRecorder()
	s.handleGitBindings(w, accessRequest(sess.User, "PUT", "/git/bindings", string(binding)), sess)
	if w.Code != 200 {
		t.Fatalf("bind: %d %s", w.Code, w.Body.String())
	}
	preview := func() map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleGitPushPreview(w, accessRequest(sess.User, "POST", "/git/push-preview", `{"repo":"","remote":"origin"}`), sess)
		if w.Code != 200 {
			t.Fatalf("preview: %d %s", w.Code, w.Body.String())
		}
		var p map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := preview()
	push := map[string]any{"repo": "", "remote": "origin", "ref": p["ref"], "expected_head": p["expected_head"], "expected_remote_head": p["expected_remote_head"]}
	raw, _ := json.Marshal(push)
	w = httptest.NewRecorder()
	s.handleGitPush(w, accessRequest(sess.User, "POST", "/git/push", string(raw)), sess)
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
	actual, err := exec.Command("git", "-C", remote, "rev-parse", "refs/heads/main").Output()
	if err != nil || strings.TrimSpace(string(actual)) != p["expected_head"] {
		t.Fatalf("remote not updated: %s %v", actual, err)
	}
	w = httptest.NewRecorder()
	s.handleGitFetch(w, accessRequest(sess.User, "POST", "/git/fetch", `{"repo":"","remote":"origin"}`), sess)
	if w.Code != 200 {
		t.Fatalf("fetch: %d %s", w.Code, w.Body.String())
	}
	tracked, err := exec.Command("git", "-C", ws, "rev-parse", "refs/remotes/origin/main").Output()
	if err != nil || string(tracked) != string(actual) {
		t.Fatalf("tracking ref: %s %v", tracked, err)
	}

	gitCmd(t, ws, "branch", "--set-upstream-to=origin/main", "main")
	seed := filepath.Join(t.TempDir(), "other-developer")
	if out, err := exec.Command("git", "clone", "-q", remote, seed).CombinedOutput(); err != nil {
		t.Fatalf("fixture clone: %s %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(seed, "upstream.txt"), []byte("upstream addition"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "commit", "-q", "-m", "upstream change")
	gitCmd(t, seed, "push", "-q", "origin", "main")
	w = httptest.NewRecorder()
	s.handleGitPull(w, accessRequest(sess.User, "POST", "/git/pull", `{"repo":"","remote":"origin"}`), sess)
	if w.Code != 200 {
		t.Fatalf("pull: %d %s", w.Code, w.Body.String())
	}
	if raw, err := os.ReadFile(filepath.Join(ws, "upstream.txt")); err != nil || string(raw) != "upstream addition" {
		t.Fatalf("pull did not update worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "test.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatal(err)
	}
	beforePull := authenticated
	w = httptest.NewRecorder()
	s.handleGitPull(w, accessRequest(sess.User, "POST", "/git/pull", `{"repo":"","remote":"origin"}`), sess)
	if w.Code != 409 || authenticated != beforePull {
		t.Fatal("dirty worktree reached network pull")
	}
	gitCmd(t, ws, "checkout", "--", "test.txt")
	gitCmd(t, ws, "commit", "--allow-empty", "-q", "-m", "second")
	p = preview()
	push["expected_head"] = p["expected_head"]
	push["expected_remote_head"] = p["expected_remote_head"]
	raw, _ = json.Marshal(push)
	gitCmd(t, ws, "commit", "--allow-empty", "-q", "-m", "third")
	w = httptest.NewRecorder()
	s.handleGitPush(w, accessRequest(sess.User, "POST", "/git/push", string(raw)), sess)
	if w.Code != 409 {
		t.Fatalf("stale preview accepted: %d %s", w.Code, w.Body.String())
	}
	stored, err := s.store.GitConnection(sess.User, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.ReadOnly = true
	if _, err = s.store.SaveGitConnection(stored, stored.Revision); err != nil {
		t.Fatal(err)
	}
	before := authenticated
	w = httptest.NewRecorder()
	s.handleGitPushPreview(w, accessRequest(sess.User, "POST", "/git/push-preview", `{"repo":"","remote":"origin"}`), sess)
	if w.Code != 409 || authenticated != before {
		t.Fatal("read-only connection attempted push")
	}
	gitCmd(t, ws, "remote", "set-url", "origin", "https://changed.example.com/repo.git")
	w = httptest.NewRecorder()
	s.handleGitFetch(w, accessRequest(sess.User, "POST", "/git/fetch", `{"repo":"","remote":"origin"}`), sess)
	if w.Code != 409 || authenticated != before {
		t.Fatal("changed target reused binding")
	}

	cloneBody, _ := json.Marshal(map[string]any{"connection_id": c.ID, "url": upstream.URL + "/repo.git", "directory": "cloned project"})
	w = httptest.NewRecorder()
	s.handleGitClone(w, accessRequest(sess.User, "POST", "/git/clone", string(cloneBody)), sess)
	if w.Code != 201 {
		t.Fatalf("clone: %d %s", w.Code, w.Body.String())
	}
	cloned := filepath.Join(ws, "cloned project")
	if data, err := os.ReadFile(filepath.Join(cloned, "upstream.txt")); err != nil || string(data) != "upstream addition" {
		t.Fatalf("clone content: %v", err)
	}
	configURL, err := exec.Command("git", "-C", cloned, "remote", "get-url", "origin").Output()
	if err != nil || strings.TrimSpace(string(configURL)) != upstream.URL+"/repo.git" {
		t.Fatalf("clone persisted temporary grant: %s %v", configURL, err)
	}
	found := false
	for _, repo := range s.repoRoots(ws) {
		found = found || repo == "cloned project"
	}
	if !found {
		t.Fatal("nested clone inside root repository not discovered")
	}
	beforeClone := authenticated
	w = httptest.NewRecorder()
	s.handleGitClone(w, accessRequest(sess.User, "POST", "/git/clone", string(cloneBody)), sess)
	if w.Code != 409 || authenticated != beforeClone {
		t.Fatal("existing directory was overwritten or contacted upstream")
	}
	if authenticated == 0 {
		t.Fatal("no upstream requests")
	}
}
