package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/clientidentity"
	"agentbox/internal/store"
)

func TestDraftScopeSeparatesInstanceAndUserLifetime(t *testing.T) {
	s, _ := newTestServer(t)
	u := store.User{Name: "alice", CreatedAt: time.Now().UTC()}
	read := func(server *Server, user store.User) string {
		r := httptest.NewRequest("GET", "/api/me", nil).WithContext(context.WithValue(t.Context(), ctxUser, user))
		w := httptest.NewRecorder()
		server.handleMe(w, r)
		var v struct {
			Scope    string `json:"draft_scope"`
			Protocol int    `json:"draft_protocol"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Protocol != 1 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		return v.Scope
	}
	scope := read(s, u)
	if len(scope) != 64 {
		t.Fatal("missing scope", scope)
	}
	if read(&Server{cfg: s.cfg, store: s.store}, u) != scope {
		t.Fatal("restart changed draft owner")
	}
	v := u
	v.Name = "bob"
	if read(s, v) == scope {
		t.Fatal("users share drafts")
	}
	v = u
	v.CreatedAt = v.CreatedAt.Add(time.Second)
	if read(s, v) == scope {
		t.Fatal("recreated user inherits drafts")
	}
	other, _ := newTestServer(t)
	if read(other, u) == scope {
		t.Fatal("restored/different instance inherits drafts")
	}
	broken, _ := newTestServer(t)
	if err := os.WriteFile(filepath.Join(broken.cfg.DataDir, clientidentity.File), []byte("invalid identity"), 0600); err != nil {
		t.Fatal(err)
	}
	if read(broken, u) != "" {
		t.Fatal("identity error enabled persistent drafts")
	}
}

func TestDraftHistoryPinsNavigableEmptyThreadWithoutMessages(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	read := func(query string) map[string]json.RawMessage {
		w := httptest.NewRecorder()
		s.handleHistory(w, httptest.NewRequest("GET", "/history"+query, nil), sess)
		var out map[string]json.RawMessage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return out
	}
	read("")
	if s.activeThread(sess) != "" {
		t.Fatal("legacy history unexpectedly allocated a thread")
	}
	first := read("?draft_context=1")
	var id string
	_ = json.Unmarshal(first["active_thread"], &id)
	if !threadIDRe.MatchString(id) {
		t.Fatal(first)
	}
	if rows := s.listThreads(sess); len(rows) != 1 || rows[0].ID != id || rows[0].Turns != 0 {
		t.Fatal("empty draft thread is not navigable", rows)
	}
	if firstMessage, turns, titled := s.threadTitleSource(sess, id); firstMessage != "" || turns != 0 || titled {
		t.Fatal("draft context became a model message")
	}
	w := httptest.NewRecorder()
	s.handleThreadNew(w, httptest.NewRequest("POST", "/threads", nil), sess)
	var created struct {
		Created bool `json:"created"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Created {
		t.Fatal("duplicate empty thread", w.Body.String())
	}
	second := read("?draft_context=1")
	if string(second["active_thread"]) != string(first["active_thread"]) {
		t.Fatal("reload allocated another draft context")
	}
	other := newThreadID(time.Now().Add(time.Second))
	if err := s.setActiveThread(sess, other); err != nil {
		t.Fatal(err)
	}
	third := read("?draft_context=1")
	if string(third["active_thread"]) == string(first["active_thread"]) {
		t.Fatal("distinct empty threads share draft context")
	}
}

func TestAttachmentValidationIsScopedAndRejectsMissingOrLinkedFiles(t *testing.T) {
	s, sess := newTestServer(t)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		if err := s.store.CreateUser(store.User{Name: name, Role: store.RoleUser}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.CreateToken(name, name); err != nil {
			t.Fatal(err)
		}
	}
	area := filepath.Join(s.cfg.DataDir, "users", sess.User, "shared", ".file")
	if err := os.MkdirAll(area, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(area, "file.txt"), []byte("private content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(area, "file.txt"), filepath.Join(area, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(area, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(token string, paths []string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"paths": paths})
		r := httptest.NewRequest("POST", "/api/sessions/"+sess.ID+"/attachments/validate", bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request("alice", []string{"/shared/.file/file.txt", "/shared/.file/missing.txt", "/shared/.file/link.txt", "/shared/.file/directory"})
	var result struct {
		Valid []bool `json:"valid"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Valid) != 4 || !result.Valid[0] || result.Valid[1] || result.Valid[2] || result.Valid[3] {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private content") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("validation leaked or cached file data")
	}
	for _, token := range []string{"", "bob"} {
		if w := request(token, []string{"/shared/.file/file.txt"}); w.Code < 400 {
			t.Fatal("attachment ownership bypass", token, w.Code)
		}
	}
	for _, path := range []string{"/shared/.file/../file.txt", "/workspace/file.txt", "/shared/private.txt"} {
		if w := request("alice", []string{path}); w.Code != 400 {
			t.Fatal("unsafe path accepted", path, w.Code)
		}
	}
}
