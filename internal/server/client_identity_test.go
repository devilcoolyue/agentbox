package server

import (
	"agentbox/internal/clientidentity"
	"agentbox/internal/syncclient"
	"agentbox/internal/syncproto"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestClientIdentityPinsAuthenticatedServerAndUser(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	remote, err := syncclient.NewRemote(server.URL, "client-fixture-alice", false)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	identity, err := remote.Identity(t.Context())
	if err != nil || identity.User != "alice" || identity.Features["sync"] != 0 {
		t.Fatal(identity, err)
	}
	persisted, err := clientidentity.LoadOrCreate(s.cfg.DataDir)
	if err != nil || persisted != identity.ServerID {
		t.Fatal("not durable", err)
	}
	base, _ := syncproto.NormalizeServer(server.URL)
	binding := syncproto.Binding{Version: 1, Server: base, ServerID: identity.ServerID, User: "alice", Workspace: sess.ID, Project: "fixture", ProjectPath: ".", LocalID: syncproto.HashBytes([]byte("local"))}
	pinned, err := remote.ForBinding(t.Context(), binding)
	if err != nil {
		t.Fatal(err)
	}
	// Identity is checked before project lookup or lease acquisition.
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/sync/lease", nil)
	request.Header.Set("Authorization", "Bearer client-fixture-alice")
	request.Header.Set("X-Agentbox-Server-ID", syncproto.HashBytes([]byte("another instance")))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 409 || w.Header().Get("X-Agentbox-Server-ID") != identity.ServerID {
		t.Fatal(w.Code, w.Body.String())
	}
	binding.User = "bob"
	if _, err = remote.ForBinding(t.Context(), binding); err != syncclient.ErrBinding {
		t.Fatal("wrong user bound", err)
	}
	binding.User = "alice"
	binding.ServerID = syncproto.HashBytes([]byte("another instance"))
	if _, err = remote.ForBinding(t.Context(), binding); err != syncclient.ErrBinding {
		t.Fatal("wrong server bound", err)
	}
	// Replacing the HTTP peer's identity after pinning must stop subsequent work.
	original := s.clientInstanceID
	s.clientInstanceID = binding.ServerID
	if _, err = pinned.Acquire(t.Context(), sess.ID, "fixture", "device"); err != syncclient.ErrBinding {
		t.Fatal("changed peer accepted", err)
	}
	s.clientInstanceID = original
}
func TestClientIdentityFailureKeepsExistingAPIAvailable(t *testing.T) {
	s, _, handler := clientTestServer(t)
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, clientidentity.File), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	w := clientRequest(handler, "alice", "GET", "/api/clients/capabilities", "")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = clientRequest(handler, "alice", "GET", "/api/me", "")
	if w.Code != 200 {
		t.Fatal("identity failure broke legacy API", w.Code)
	}
	var me map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
}
