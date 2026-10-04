package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"agentbox/internal/syncproto"
)

func TestDesktopSyncCapabilityRequiresExplicitAdminOptIn(t *testing.T) {
	s, _, handler := clientTestServer(t)
	s.tunnels = newTunnelHub()
	if err := s.store.CreateToken("client-fixture-root", "root"); err != nil {
		t.Fatal(err)
	}
	read := func() syncproto.ServerIdentity {
		t.Helper()
		response := clientRequest(handler, "alice", http.MethodGet, "/api/clients/capabilities", "")
		var identity syncproto.ServerIdentity
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &identity) != nil {
			t.Fatal("capabilities", response.Code, response.Body.String())
		}
		return identity
	}
	initial := read()
	if initial.Features["sync"] != 0 {
		t.Fatal("default enabled sync")
	}
	if response := clientRequest(handler, "alice", http.MethodPut, "/api/settings", `{"desktop_sync_enabled":true}`); response.Code != http.StatusForbidden {
		t.Fatal("regular user changed rollout", response.Code)
	}
	for _, enabled := range []bool{true, false} {
		body, _ := json.Marshal(map[string]bool{"desktop_sync_enabled": enabled})
		response := clientRequest(handler, "root", http.MethodPut, "/api/settings", string(body))
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		current := read()
		if (current.Features["sync"] == 1) != enabled || current.ServerID != initial.ServerID || current.User != initial.User {
			t.Fatal("rollout changed server/owner identity or wrong capability", current)
		}
		if current.Features["sync_recovery_inspect"] != 1 || current.Features["sync_recovery_gc"] != 1 || current.Features["project_terminals"] != 1 {
			t.Fatal("sync rollout disabled independent terminal/recovery capabilities", current.Features)
		}
	}
}
