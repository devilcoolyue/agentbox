package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func (f *receiptFixture) fileMentions(t *testing.T, user, query string) (int, fileMentionsView) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/sessions/"+f.sess.ID+"/files/search?q="+url.QueryEscape(query), nil)
	r.Header.Set("Authorization", "Bearer "+user)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	var view fileMentionsView
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(err, w.Body.String())
		}
	}
	return w.Code, view
}

// "@" suggestions read names from the workspace on the host, so they work with
// the container stopped; a short cache means new files show up seconds later.
func TestFileMentionsSearchWorkspace(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	f.sess.Status = store.StatusStopped
	if err := f.s.store.Put(f.sess); err != nil {
		t.Fatal(err)
	}
	ws := f.s.workspaceDir(f.sess)
	for name, body := range map[string]string{"src/demo.py": "x", "src/my notes.md": "", ".gitignore": "out/\n", "out/bundle.js": ""} {
		p := filepath.Join(ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, view := f.fileMentions(t, "alice", "demo")
	if code != 200 || len(view.Files) != 1 || view.Files[0] != (fileMention{Path: "src/demo.py"}) || view.Truncated {
		t.Fatal(code, view)
	}
	if _, view = f.fileMentions(t, "alice", ""); len(view.Files) != 2 || view.Files[0] != (fileMention{Path: "src", Dir: true}) || view.Files[1].Path != ".gitignore" {
		t.Fatalf("top level, ignored folder left out: %+v", view.Files)
	}
	if _, view = f.fileMentions(t, "alice", "src/"); len(view.Files) != 2 || view.Files[1].Path != "src/my notes.md" {
		t.Fatalf("folder listing: %+v", view.Files)
	}
	if _, view = f.fileMentions(t, "alice", "bundle"); len(view.Files) != 0 {
		t.Fatalf(".gitignore'd output listed: %+v", view.Files)
	}
	if code, _ := f.fileMentions(t, "bob", "demo"); code == 200 {
		t.Fatal("another user's workspace searched")
	}
	r := httptest.NewRequest("GET", "/api/sessions/"+f.sess.ID+"/files/search?q=a&limit=1000", nil)
	r.Header.Set("Authorization", "Bearer alice")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("unbounded limit accepted: %d", w.Code)
	}
}
