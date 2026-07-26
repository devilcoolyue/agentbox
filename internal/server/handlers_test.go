package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// newTestServer builds a Server backed by a temp data dir and SQLite store,
// with no Docker manager — enough to exercise the file and auth handlers
// directly (bypassing the auth/withSession middleware).
func newTestServer(t *testing.T) (*Server, store.Session) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Server{
		cfg:    &config.Config{DataDir: dataDir, MaxUploadMB: 10},
		store:  st,
		logins: newLoginGuard(),
	}
	sess := store.Session{ID: "s1", User: "alice", Name: "demo"}
	if err := os.MkdirAll(s.workspaceDir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	return s, sess
}

func TestHandleFileGetRejectsSymlink(t *testing.T) {
	s, sess := newTestServer(t)
	ws := s.workspaceDir(sess)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/file?path=escape/secret.txt", nil)
	w := httptest.NewRecorder()
	s.handleFileGet(w, req, sess)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if strings.Contains(w.Body.String(), "top secret") {
		t.Fatalf("symlink escape leaked host file content: %s", w.Body.String())
	}
}

func TestHandleFilesRejectsSymlinkAndBadScope(t *testing.T) {
	s, sess := newTestServer(t)
	ws := s.workspaceDir(sess)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/files?path=escape", nil)
	w := httptest.NewRecorder()
	s.handleFiles(w, req, sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("listing through symlink: status = %d, want 400", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/sessions/s1/files?scope=bogus", nil)
	w = httptest.NewRecorder()
	s.handleFiles(w, req, sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope: status = %d, want 400", w.Code)
	}
}

func TestHandleUploadClearReplacesAtomically(t *testing.T) {
	s, sess := newTestServer(t)
	ws := s.workspaceDir(sess)
	if err := os.WriteFile(filepath.Join(ws, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("clear", "1")
	fw, err := mw.CreateFormFile("file", "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("new content"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.handleUpload(w, req, sess)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("clear=1 did not remove the old file: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(ws, "new.txt")); err != nil || string(raw) != "new content" {
		t.Fatalf("uploaded file = %q, %v", raw, err)
	}
	// No staging leftovers in the session dir.
	entries, _ := os.ReadDir(s.sessionDir(sess))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stage-") {
			t.Fatalf("staging artifact left behind: %s", e.Name())
		}
	}
}

func TestBearerTokenQueryOnlyOnGet(t *testing.T) {
	get := httptest.NewRequest(http.MethodGet, "/x?token=abc", nil)
	if got := bearerToken(get); got != "abc" {
		t.Fatalf("GET query token = %q, want abc", got)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(m, "/x?token=abc", nil)
		if got := bearerToken(r); got != "" {
			t.Fatalf("%s query token = %q, want empty", m, got)
		}
	}
	hdr := httptest.NewRequest(http.MethodPost, "/x?token=abc", nil)
	hdr.Header.Set("Authorization", "Bearer xyz")
	if got := bearerToken(hdr); got != "xyz" {
		t.Fatalf("header token = %q, want xyz", got)
	}
}

func TestHandleLoginRateLimitAndUnknownUser(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: hashPassword("password123")}); err != nil {
		t.Fatal(err)
	}
	old := loginFailDelay
	loginFailDelay = 0
	t.Cleanup(func() { loginFailDelay = old })

	login := func(user, pass, ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/login",
			strings.NewReader(`{"username":"`+user+`","password":"`+pass+`"}`))
		req.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		s.handleLogin(w, req)
		return w.Code
	}

	// An unknown username must not panic (dummy-hash path) and returns 401.
	if code := login("ghost", "whatever", "9.9.9.9"); code != http.StatusUnauthorized {
		t.Fatalf("unknown user status = %d, want 401", code)
	}

	// Wrong password repeated from one IP eventually gets throttled.
	for i := 0; i < loginMaxFails; i++ {
		if code := login("alice", "wrong", "1.2.3.4"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i, code)
		}
	}
	if code := login("alice", "wrong", "1.2.3.4"); code != http.StatusTooManyRequests {
		t.Fatalf("throttled status = %d, want 429", code)
	}
	// A different IP is unaffected by another IP's failures.
	if code := login("alice", "password123", "5.6.7.8"); code != http.StatusOK {
		t.Fatalf("fresh IP login status = %d, want 200", code)
	}
}

func TestHandleFileMkdirAndRename(t *testing.T) {
	s, sess := newTestServer(t)
	ws := s.workspaceDir(sess)

	mkdir := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/x/files/mkdir", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleFileMkdir(w, req, sess)
		return w.Code
	}
	rename := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/x/files/rename", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleFileRename(w, req, sess)
		return w.Code
	}

	if code := mkdir(`{"scope":"workspace","dir":"","name":"proj"}`); code != http.StatusOK {
		t.Fatalf("mkdir status = %d, want 200", code)
	}
	if fi, err := os.Stat(filepath.Join(ws, "proj")); err != nil || !fi.IsDir() {
		t.Fatalf("mkdir did not create dir: %v", err)
	}
	if code := mkdir(`{"scope":"workspace","dir":"","name":"proj"}`); code != http.StatusConflict {
		t.Fatalf("mkdir conflict status = %d, want 409", code)
	}
	if code := mkdir(`{"scope":"workspace","dir":"","name":"a/b"}`); code != http.StatusBadRequest {
		t.Fatalf("mkdir bad name status = %d, want 400", code)
	}

	if code := rename(`{"scope":"workspace","path":"proj","name":"project"}`); code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200", code)
	}
	if _, err := os.Stat(filepath.Join(ws, "project")); err != nil {
		t.Fatalf("rename target missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "proj")); !os.IsNotExist(err) {
		t.Fatalf("rename source still exists")
	}
	if err := os.Mkdir(filepath.Join(ws, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := rename(`{"scope":"workspace","path":"project","name":"other"}`); code != http.StatusConflict {
		t.Fatalf("rename conflict status = %d, want 409", code)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, "escape")); err != nil {
		t.Fatal(err)
	}
	if code := rename(`{"scope":"workspace","path":"escape/x","name":"y"}`); code != http.StatusBadRequest && code != http.StatusNotFound {
		t.Fatalf("rename through symlink status = %d, want 400/404", code)
	}
}

func TestHandleRenameSession(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Status = store.StatusStopped
	sess.CreatedAt, sess.UpdatedAt = time.Now(), time.Now()
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}

	rename := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/sessions/s1", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleRenameSession(w, req, sess)
		return w
	}

	if w := rename(`{"name":"重构支付模块"}`); w.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got, _ := s.store.Get("s1"); got.Name != "重构支付模块" {
		t.Fatalf("stored name = %q, want 重构支付模块", got.Name)
	}
	if w := rename(`{"name":"   "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("blank name status = %d, want 400", w.Code)
	}
	if w := rename(`{"name":"` + strings.Repeat("字", 65) + `"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("overlong name status = %d, want 400", w.Code)
	}
	// 校验失败不得改动已存的名字
	if got, _ := s.store.Get("s1"); got.Name != "重构支付模块" {
		t.Fatalf("name changed by a rejected rename: %q", got.Name)
	}
}
