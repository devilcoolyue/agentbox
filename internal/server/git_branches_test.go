package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitBranchLifecycleAndGuards(t *testing.T) {
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws, "add", "-A")
	gitCmd(t, ws, "commit", "-q", "-m", "first")
	head := func() string {
		t.Helper()
		out, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	initial := head()
	action := func(act, name, branch, target string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"repo": "", "action": act, "name": name, "expected_head": head(), "expected_branch": branch, "target_head": target})
		w := httptest.NewRecorder()
		s.handleGitBranchAction(w, httptest.NewRequest("POST", "/git/branches", strings.NewReader(string(raw))), sess)
		return w
	}
	if w := action("create", "feature/example", "main", ""); w.Code != 200 {
		t.Fatalf("create: %s", w.Body.String())
	}
	if w := action("switch", "main", "main", initial); w.Code != 409 {
		t.Fatal("stale current branch accepted")
	}
	if w := action("switch", "main", "feature/example", initial); w.Code != 200 {
		t.Fatalf("switch: %s", w.Body.String())
	}
	if w := action("delete", "feature/example", "main", initial); w.Code != 200 {
		t.Fatalf("delete merged: %s", w.Body.String())
	}
	if w := action("delete", "main", "main", initial); w.Code != 409 {
		t.Fatal("deleted current branch")
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	if w := action("create", "feature/dirty", "main", ""); w.Code != 409 {
		t.Fatal("dirty worktree switched")
	}
	gitCmd(t, ws, "checkout", "--", "a.txt")
	if w := action("create", "feature/unique", "main", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	gitCmd(t, ws, "commit", "--allow-empty", "-q", "-m", "unique")
	unique := head()
	if w := action("switch", "main", "feature/unique", initial); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := action("delete", "feature/unique", "main", unique); w.Code != 409 {
		t.Fatal("unmerged commits deleted")
	}
	gitCmd(t, ws, "remote", "add", "origin", "https://git.example.com/repo.git")
	gitCmd(t, ws, "update-ref", "refs/remotes/origin/main", initial)
	if w := action("upstream", "origin/main", "main", initial); w.Code != 200 {
		t.Fatalf("upstream: %s", w.Body.String())
	}
	w := httptest.NewRecorder()
	s.handleGitBranches(w, httptest.NewRequest("GET", "/git/branches", nil), sess)
	var out struct {
		Branches []gitBranch `json:"branches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range out.Branches {
		if b.Name == "main" && b.Current && b.Upstream == "origin/main" {
			found = true
		}
	}
	if !found {
		t.Fatalf("branch list: %s", w.Body.String())
	}
}
