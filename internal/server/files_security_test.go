package server

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
)

type callbackReader struct {
	before func()
	reader io.Reader
}

func (r *callbackReader) Read(b []byte) (int, error) {
	if r.before != nil {
		f := r.before
		r.before = nil
		f()
	}
	return r.reader.Read(b)
}

func TestEditorSaveRejectsParentSwappedWhileReadingBody(t *testing.T) {
	s, sess := newTestServer(t)
	ws := s.workspaceDir(sess)
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader := &callbackReader{reader: strings.NewReader("overwrite"), before: func() {
		if err := os.Rename(filepath.Join(ws, "dir"), filepath.Join(ws, "old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(ws, "dir")); err != nil {
			t.Fatal(err)
		}
	}}
	w := httptest.NewRecorder()
	s.handleFilePut(w, httptest.NewRequest(http.MethodPut, "/file?path=dir/file", reader), sess)
	if w.Code == http.StatusOK {
		t.Fatal("save through replaced parent succeeded")
	}
	if raw, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(raw) != "outside" {
		t.Fatalf("outside changed %q %v", raw, err)
	}
}

func TestPreviewGrantRejectsReplacedGrantedDirectory(t *testing.T) {
	s, sess, ws := newPreviewServer(t)
	url := grantFor(t, s, sess, "proto/index.html")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "index.html"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(ws, "proto"), filepath.Join(ws, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "proto")); err != nil {
		t.Fatal(err)
	}
	w := servePreview(s, url)
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "outside-secret") {
		t.Fatalf("preview escaped: %d %s", w.Code, w.Body.String())
	}
}

func TestSkillEndpointsRejectLinkedClaudeDirectory(t *testing.T) {
	s, sess := newTestServer(t)
	home := s.homeDir(sess)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "skills", "demo", "SKILL.md"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) { s.handleSkillGet(w, r, sess) },
		func(w http.ResponseWriter, r *http.Request) { s.handleSkillFile(w, r, sess) },
		func(w http.ResponseWriter, r *http.Request) { s.handleSkillDelete(w, r, sess) },
	} {
		r := httptest.NewRequest(http.MethodGet, "/skills/demo?path=SKILL.md", nil)
		r.SetPathValue("name", "demo")
		w := httptest.NewRecorder()
		handle(w, r)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "outside-secret") {
			t.Fatalf("skill escaped: %d %s", w.Code, w.Body.String())
		}
	}
	if w := installSkill(t, s, sess, "session", "demo.md", "demo", []byte("overwrite")); w.Code == http.StatusOK {
		t.Fatal("install followed .claude link")
	}
	if raw, err := os.ReadFile(filepath.Join(outside, "skills", "demo", "SKILL.md")); err != nil || string(raw) != "outside-secret" {
		t.Fatalf("outside skill changed: %q %v", raw, err)
	}
}

func TestUploadArchiveRejectsExistingDestinationLink(t *testing.T) {
	s, sess := newTestServer(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.workspaceDir(sess), "target")); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	f, err := zw.Create("target/file")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "overwrite")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	f, err = mw.CreateFormFile("file", "upload.zip")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(archive.Bytes())
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.handleUpload(w, r, sess)
	if w.Code == http.StatusOK {
		t.Fatal("upload followed destination link")
	}
	if raw, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(raw) != "keep" {
		t.Fatalf("outside overwritten %q %v", raw, err)
	}
}

func TestCredentialSyncRejectsLinkedHomeAndPredictableTemp(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = config.AgentClaude
	pool := t.TempDir()
	home := filepath.Join(s.homeDir(sess), ".claude")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	name := ".credentials.json"
	old := time.Now().Add(-time.Hour)
	if err := os.WriteFile(filepath.Join(home, name), []byte(`{"refreshToken":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(home, name), old, old); err != nil {
		t.Fatal(err)
	}
	fresh := `{"refreshToken":"new"}`
	if err := os.WriteFile(filepath.Join(pool, name), []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, name+".tmp")); err != nil {
		t.Fatal(err)
	}
	acct := config.Account{CredentialsDir: pool, Type: config.AgentClaude}
	s.syncRotatingCred(acct, sess)
	if raw, _ := os.ReadFile(filepath.Join(home, name)); string(raw) != fresh {
		t.Fatalf("not synchronized: %q", raw)
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "keep" {
		t.Fatal("predictable temp symlink overwritten")
	}
	// Reverse direction: a newer symlinked home credential must not be read
	// into the account pool, regardless of its timestamp.
	if err := os.Remove(filepath.Join(home, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(pool, name), old, old); err != nil {
		t.Fatal(err)
	}
	s.syncRotatingCred(acct, sess)
	if raw, _ := os.ReadFile(filepath.Join(pool, name)); string(raw) != fresh {
		t.Fatalf("pool read outside content: %q", raw)
	}
}
