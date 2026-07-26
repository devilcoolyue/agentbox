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

func TestGitStatusDiffCommitDiscard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, sess := newTestServer(t)
	ws := s.workspaceDir(sess)

	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	// Modify a tracked file and add an untracked one.
	_ = os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello world\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "b.txt"), []byte("new file\n"), 0o644)

	type statusResp struct {
		IsRepo bool      `json:"is_repo"`
		Files  []gitFile `json:"files"`
	}
	getStatus := func() statusResp {
		w := httptest.NewRecorder()
		s.handleGitStatus(w, httptest.NewRequest(http.MethodGet, "/g", nil), sess)
		if w.Code != http.StatusOK {
			t.Fatalf("status code = %d", w.Code)
		}
		var out statusResp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	st := getStatus()
	if !st.IsRepo || len(st.Files) != 2 {
		t.Fatalf("status = %+v, want repo with 2 changed files", st)
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
	if st := getStatus(); len(st.Files) != 0 {
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
