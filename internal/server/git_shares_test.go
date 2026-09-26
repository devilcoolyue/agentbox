package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/store"
)

func TestSharedGitCredentialsReadOnlyAndGrantRevocation(t *testing.T) {
	s, _ := newTestServer(t)
	for _, u := range []store.User{{Name: "root", Role: store.RoleAdmin, PassHash: "fixture"}, {Name: "alice", Role: store.RoleUser, PassHash: "fixture"}} {
		if err := s.store.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	c := createGitConnection(t, s, "root", "https://git.example.com", false)
	if err := s.store.SetGitShares("root", c.ID, c.Revision, []store.GitShare{{User: "alice"}}); err != nil {
		t.Fatal(err)
	}
	viewer, err := s.store.GitConnectionFor("alice", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, secret, err := s.gitCredential(t.Context(), viewer)
	if err != nil || secret != "synthetic-secret" || !credential.ReadOnly || credential.Actor != "alice" {
		t.Fatalf("execution identity/permission lost: %+v %v", credential, err)
	}
	if _, _, err := s.gitTransport(t.Context(), viewer, "https://git.example.com/group/repo", true, "refs/heads/main", strings.Repeat("0", 40), strings.Repeat("1", 40)); err == nil {
		t.Fatal("read-only shared connection obtained write transport")
	}
	w := httptest.NewRecorder()
	s.handleGitConnections(w, accessRequest("alice", "GET", "/connections", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-secret") || !strings.Contains(w.Body.String(), `"managed":false`) {
		t.Fatalf("shared metadata: %s", w.Body.String())
	}
	request := accessRequest("alice", "PATCH", "/connections", `{"revision":2,"read_only":false}`)
	request.SetPathValue("connection", c.ID)
	w = httptest.NewRecorder()
	s.handleGitConnection(w, request)
	if w.Code != 404 {
		t.Fatal("recipient edited owner credential")
	}
	if err = s.store.SetGitShares("root", c.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.gitCredential(t.Context(), viewer); err == nil {
		t.Fatal("cached viewer credential bypassed revoke")
	}
	// ACL handler also enforces admin through route middleware, and the store
	// performs an independent transactional role/ownership check.
	body, _ := json.Marshal(map[string]any{"revision": 3, "users": []store.GitShare{{User: "alice", Write: true}}})
	req := accessRequest("root", "PUT", "/shares", string(body))
	req.SetPathValue("connection", c.ID)
	w = httptest.NewRecorder()
	s.handleGitShares(w, req)
	if w.Code != 200 {
		t.Fatalf("ACL save: %s", w.Body.String())
	}
}
