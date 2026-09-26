package server

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"agentbox/internal/store"
)

func TestGitRemoteManagementAndBindingGuard(t *testing.T) {
	s, sess := newGitTestServer(t)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q")
	c := createGitConnection(t, s, sess.User, "https://git.example.com", true)
	call := func(action, url, expected string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"repo": "", "name": "origin", "action": action, "url": url, "expected_url": expected, "connection_id": c.ID})
		w := httptest.NewRecorder()
		s.handleGitRemoteEdit(w, httptest.NewRequest("POST", "/remotes", strings.NewReader(string(raw))), sess)
		return w
	}
	first := "https://git.example.com/team/first.git"
	second := "https://git.example.com/team/second.git"
	if w := call("add", first, ""); w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	if w := call("add", first, ""); w.Code != 409 {
		t.Fatal("duplicate remote accepted")
	}
	b := store.GitBinding{SessionID: sess.ID, Remote: "origin", URL: first, ConnectionID: c.ID}
	if err := s.store.SaveGitBinding(sess.User, b, 0); err != nil {
		t.Fatal(err)
	}
	if w := call("update", second, first); w.Code != 409 {
		t.Fatal("bound remote changed without unbinding")
	}
	b.ConnectionID = ""
	if err := s.store.SaveGitBinding(sess.User, b, 1); err != nil {
		t.Fatal(err)
	}
	if w := call("update", second, "stale-url"); w.Code != 409 {
		t.Fatal("stale remote update accepted")
	}
	if w := call("update", "https://evil.example/repo.git", first); w.Code != 400 {
		t.Fatal("wrong platform URL accepted")
	}
	if w := call("update", second, first); w.Code != 200 {
		t.Fatalf("update: %s", w.Body.String())
	}
	got, err := exec.Command("git", "-C", ws, "remote", "get-url", "origin").Output()
	if err != nil || strings.TrimSpace(string(got)) != second {
		t.Fatalf("URL not updated: %s %v", got, err)
	}
	gitCmd(t, ws, "config", "remote.origin.pushurl", first)
	if w := call("update", first, second); w.Code != 409 {
		t.Fatal("independent pushurl silently retained")
	}
	if w := call("remove", "", second); w.Code != 200 {
		t.Fatalf("remove: %s", w.Body.String())
	}
	got, err = exec.Command("git", "-C", ws, "remote").Output()
	if err != nil || len(got) != 0 {
		t.Fatalf("remote not removed: %s %v", got, err)
	}
}
