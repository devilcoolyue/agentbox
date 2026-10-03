package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
	"github.com/gorilla/websocket"
)

func clientTestServer(t *testing.T) (*Server, store.Session, http.Handler) {
	t.Helper()
	s, sess := accessTestServer(t)
	sess.AccountID = "shared"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob"} {
		if err := s.store.CreateToken("client-fixture-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	sess, _ = s.store.Get(sess.ID)
	return s, sess, s.Handler()
}
func clientRequest(handler http.Handler, user, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		r.Header.Set("Authorization", "Bearer client-fixture-"+user)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func TestClientRoutesOwnershipAndNoFilesystemMigration(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	if err := os.WriteFile(filepath.Join(s.workspaceDir(sess), "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/clients/capabilities", "/api/sessions/s1/client-projects", "/api/sessions/s1/client-terminals"} {
		if w := clientRequest(handler, "", "GET", route, ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", route, w.Code)
		}
	}
	if w := clientRequest(handler, "bob", "POST", "/api/sessions/s1/client-projects", `{"name":"Root","path":"."}`); w.Code != 404 {
		t.Fatalf("cross-user %d %s", w.Code, w.Body.String())
	}
	if w := clientRequest(handler, "", "POST", "/api/sessions/s1/client-projects?token=client-fixture-alice", `{"name":"Root","path":"."}`); w.Code != 401 {
		t.Fatalf("write token query accepted: %d", w.Code)
	}
	w := clientRequest(handler, "alice", "POST", "/api/sessions/s1/client-projects", `{"name":"Root","path":"."}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var project store.ClientProject
	json.Unmarshal(w.Body.Bytes(), &project)
	w = clientRequest(handler, "alice", "POST", "/api/sessions/s1/client-terminals", `{"project_id":"`+project.ID+`","kind":"shell"}`)
	if w.Code != 201 {
		t.Fatalf("terminal metadata unexpectedly needed Docker: %d %s", w.Code, w.Body.String())
	}
	var terminal store.ClientTerminal
	json.Unmarshal(w.Body.Bytes(), &terminal)
	w = clientRequest(handler, "bob", "DELETE", "/api/sessions/s1/client-terminals/"+terminal.ID, "")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = clientRequest(handler, "alice", "DELETE", "/api/sessions/s1/client-projects/"+project.ID+"?revision=1", "")
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	w = clientRequest(handler, "alice", "DELETE", "/api/sessions/s1/client-terminals/"+terminal.ID, "")
	if w.Code != 200 {
		t.Fatalf("close not-started terminal: %d %s", w.Code, w.Body.String())
	}
	w = clientRequest(handler, "alice", "DELETE", "/api/sessions/s1/client-projects/"+project.ID+"?revision="+strconv.FormatInt(project.Revision, 10), "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if body, err := os.ReadFile(filepath.Join(s.workspaceDir(sess), "keep.txt")); err != nil || string(body) != "keep" {
		t.Fatal("project deletion changed workspace files", err)
	}
	current, _ := s.store.Get(sess.ID)
	if current != sess {
		t.Fatal("project metadata changed session")
	}
}

func TestClientProjectsRejectLinksBadPathsAndRevokedTerminal(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(s.workspaceDir(sess), "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../x", "/workspace", "escape", "missing"} {
		body, _ := json.Marshal(map[string]string{"name": "P", "path": path})
		w := clientRequest(handler, "alice", "POST", "/api/sessions/s1/client-projects", string(body))
		if w.Code != 400 {
			t.Fatalf("path %q: %d %s", path, w.Code, w.Body.String())
		}
	}
	p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	sess.AccountID = "selected"
	if err = s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	w := clientRequest(handler, "alice", "POST", "/api/sessions/s1/client-terminals", `{"project_id":"`+p.ID+`","kind":"agent"}`)
	if w.Code != 403 {
		t.Fatalf("revoked account got terminal: %d", w.Code)
	}
	w = clientRequest(handler, "alice", "GET", "/api/sessions/s1/client-projects", "")
	if w.Code != 200 {
		t.Fatal("revocation should not hide file metadata")
	}
}

func TestClientPairingIndependentOfTunnelAndSingleUse(t *testing.T) {
	s, _, handler := clientTestServer(t)
	s.pairs = newPairStore()
	w := clientRequest(handler, "alice", "POST", "/api/clients/pair", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var pair struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &pair)
	if _, ok := s.pairs.redeem(pair.Code); ok {
		t.Fatal("desktop code usable as tunnel code")
	}
	w = clientRequest(handler, "", "POST", "/api/clients/pair/redeem", `{"code":"`+pair.Code+`"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var login struct {
		Token string `json:"token"`
		User  string `json:"user"`
	}
	json.Unmarshal(w.Body.Bytes(), &login)
	if login.User != "alice" || login.Token == "" {
		t.Fatal("invalid pairing identity")
	}
	w = clientRequest(handler, "", "POST", "/api/clients/pair/redeem", `{"code":"`+pair.Code+`"}`)
	if w.Code != 401 {
		t.Fatal("replayed pairing", w.Code)
	}
	if w = clientRequest(handler, "alice", "POST", "/api/tunnel/pair", ""); w.Code != 403 {
		t.Fatal("desktop pairing enabled tunnel")
	}
}

func TestClientTerminalQuotaAndClosingRejectBeforeDocker(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := s.store.CreateClientTerminal(sess.ID, p.ID, "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.Grant(sess.User, -1000, "synthetic-debt", "", "test"); err != nil {
		t.Fatal(err)
	}
	w := clientRequest(handler, "alice", "POST", "/api/sessions/s1/client-terminals", `{"project_id":"`+p.ID+`","kind":"shell"}`)
	if w.Code != 403 {
		t.Fatal("quota creation gate", w.Code)
	}
	s.upgrader = websocket.Upgrader{CheckOrigin: sameHostOrigin}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	address := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/sessions/s1/client-terminals/" + terminal.ID + "/stream"
	headers := http.Header{"Authorization": []string{"Bearer client-fixture-alice"}}
	conn, _, err := websocket.DefaultDialer.Dial(address, headers)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	conn.Close()
	if !websocket.IsCloseError(err, closeQuota) {
		t.Fatalf("quota close: %v", err)
	}
	if err = s.store.BeginCloseClientTerminal(sess.ID, terminal.ID); err != nil {
		t.Fatal(err)
	}
	conn, response, err := websocket.DefaultDialer.Dial(address, headers)
	if conn != nil {
		conn.Close()
	}
	if response != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != 409 {
		t.Fatalf("closing terminal was opened: %v %+v", err, response)
	}
}

func TestClientManifestIsReadOnlyAndScoped(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	if err := os.WriteFile(filepath.Join(s.workspaceDir(sess), "hello.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	w := clientRequest(handler, "alice", "GET", "/api/sessions/s1/sync/manifest?project="+p.ID, "")
	if w.Code != 200 {
		t.Fatalf("manifest: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello.txt") || !strings.Contains(w.Body.String(), "windows_issues") {
		t.Fatal(w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("manifest cached")
	}
	if w = clientRequest(handler, "bob", "GET", "/api/sessions/s1/sync/manifest?project="+p.ID, ""); w.Code != 404 {
		t.Fatalf("cross-user manifest: %d", w.Code)
	}
}
