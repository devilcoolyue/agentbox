package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitStateWithTrackingAndUnusualPaths(t *testing.T) {
	s, sess := newGitTestServer(t)
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(ws, "original"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws, "add", "-A")
	gitCmd(t, ws, "commit", "-q", "-m", "initial")
	gitCmd(t, ws, "remote", "add", "origin", "https://fixture-user:fixture-secret@git.example.com/group/repo.git?token=fixture-query#secret")
	gitCmd(t, ws, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitCmd(t, ws, "branch", "--set-upstream-to=origin/main")
	gitCmd(t, ws, "commit", "--allow-empty", "-q", "-m", "ahead")
	name := "中文 \"quoted\" -> path\nline.txt"
	gitCmd(t, ws, "mv", "original", name)
	newName := "new \t file\n.txt"
	if err := os.WriteFile(filepath.Join(ws, newName), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleGitStatus(w, httptest.NewRequest("GET", "/git/status", nil), sess)
	if w.Code != 200 {
		t.Fatalf("status: %s", w.Body.String())
	}
	var state struct {
		gitBranchState
		Files   []gitFile   `json:"files"`
		Remotes []gitRemote `json:"remotes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Upstream != "origin/main" || state.Ahead != 1 || state.Behind != 0 || !state.TrackingKnown {
		t.Fatalf("tracking: %+v", state)
	}
	if len(state.Remotes) != 1 || state.Remotes[0].URL != "https://git.example.com/group/repo.git" || strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "fixture-query") {
		t.Fatalf("redaction: %s", w.Body.String())
	}
	paths := map[string]string{}
	for _, f := range state.Files {
		paths[f.Path] = f.Status
	}
	if paths[name] != "R " || paths[newName] != "??" {
		t.Fatalf("paths corrupted: %#v", paths)
	}
	gitCmd(t, ws, "checkout", "--detach", "-q")
	w = httptest.NewRecorder()
	s.handleGitStatus(w, httptest.NewRequest("GET", "/git/status", nil), sess)
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Detached || state.Branch != "" || len(state.Head) != 40 {
		t.Fatalf("detached: %+v", state)
	}
}

func TestGitStateUnbornAndConflict(t *testing.T) {
	st, files := parseGitState("# branch.oid (initial)\x00# branch.head main\x00? first file\x00")
	if !st.Unborn || st.Head != "" || st.Branch != "main" || len(files) != 1 {
		t.Fatalf("unborn: %+v %+v", st, files)
	}
	_, files = parseGitState("u UU N... 100644 100644 100644 100644 a b c conflict name\x00")
	if len(files) != 1 || files[0].Path != "conflict name" || files[0].Status != "UU" {
		t.Fatalf("conflict: %+v", files)
	}
}

func TestGitRemoteDisplay(t *testing.T) {
	for _, raw := range []string{"https://user:secret@host/group/repo?secret=yes#secret", "ssh://secret@host/group/repo", "secret@host:group/repo", "ext::secret", "/secret/path"} {
		if strings.Contains(displayGitURL(raw), "secret") {
			t.Errorf("leaked %q as %q", raw, displayGitURL(raw))
		}
	}
}
