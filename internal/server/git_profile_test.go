package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitProfileSelfServiceAndValidation(t *testing.T) {
	s, _ := newTestServer(t)
	for _, payload := range []string{
		`{"name":"Alice","email":"alice@example.com"}`,
		`{"name":" 王小明 ","email":" 123+name@users.noreply.github.com "}`,
	} {
		w := httptest.NewRecorder()
		s.handleGitProfile(w, accessRequest("alice", "PUT", "/api/me/git", payload))
		if w.Code != 200 {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
	}
	for _, payload := range []string{
		`{"name":"Alice\nInjected","email":"alice@example.com"}`,
		`{"name":"Alice","email":"Name <alice@example.com>"}`,
		`{"name":"","email":"alice@example.com"}`,
		`{"name":"Alice","email":"bad"}`,
		`{"name":"Alice","email":"alice@example.com","user":"bob"}`,
		`{"name":"A\u0000lice","email":"alice@example.com"}`,
	} {
		w := httptest.NewRecorder()
		s.handleGitProfile(w, accessRequest("alice", "PUT", "/api/me/git", payload))
		if w.Code != 400 {
			t.Fatalf("accepted %q: %d", payload, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.handleGitProfile(w, accessRequest("bob", "GET", "/api/me/git", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), "小明") {
		t.Fatalf("isolation: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.auth(http.HandlerFunc(s.handleGitProfile)).ServeHTTP(w, httptest.NewRequest("GET", "/api/me/git", nil))
	if w.Code != 401 {
		t.Fatalf("anonymous status=%d", w.Code)
	}
}

func TestGitCommitIdentityAndLocalOnly(t *testing.T) {
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q")
	// Repository identity must not override the selected user's web identity.
	gitCmd(t, ws, "config", "author.name", "Wrong author")
	gitCmd(t, ws, "config", "author.email", "wrong@example.com")
	gitCmd(t, ws, "config", "committer.name", "Wrong committer")
	gitCmd(t, ws, "config", "committer.email", "wrong@example.com")
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare", "-q")
	gitCmd(t, ws, "remote", "add", "origin", remote)
	if _, err := s.store.SetGitProfile(sess.User, "Alice Example", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleGitCommit(w, httptest.NewRequest("POST", "/git/commit", strings.NewReader(`{"message":"local commit"}`)), sess)
	if w.Code != 200 {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		SHA    string `json:"sha"`
		Pushed *bool  `json:"pushed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.SHA) != 40 || result.Pushed == nil || *result.Pushed {
		t.Fatalf("result: %s", w.Body.String())
	}
	out, err := exec.Command("git", "-C", ws, "show", "-s", "--format=%an <%ae>|%cn <%ce>", result.SHA).Output()
	if err != nil || strings.TrimSpace(string(out)) != "Alice Example <alice@example.com>|Alice Example <alice@example.com>" {
		t.Fatalf("identity: %s %v", out, err)
	}
	out, err = exec.Command("git", "-C", remote, "for-each-ref").Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("remote modified: %s %v", out, err)
	}
}
