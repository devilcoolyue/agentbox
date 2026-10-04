package syncclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/syncproto"
)

func TestDesktopOrphanMaintenanceRemainsAvailableWithoutEnablingSync(t *testing.T) {
	serverID := syncproto.HashBytes([]byte("orphan-maintenance"))
	var legacy atomic.Bool
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer native-token" {
			t.Error("missing native authentication")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Agentbox-Server-ID", serverID)
		if r.URL.Path == "/api/clients/capabilities" {
			features := map[string]int{"sync": 0, "sync_recovery_gc": 0, "sync_recovery_inspect": 1}
			if legacy.Load() {
				delete(features, "sync_recovery_inspect")
			}
			json.NewEncoder(w).Encode(syncproto.ServerIdentity{Version: 1, ServerID: serverID, User: "alice", Features: features})
			return
		}
		requests.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/sessions/space/sync/recovery-operations" || r.Header.Get("X-Agentbox-Server-ID") != serverID {
			t.Error("unexpected maintenance request", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(syncproto.RecoveryOperationsPage{ServerID: serverID, Workspace: "space", Items: []syncproto.RecoveryListItem{}})
	}))
	defer server.Close()
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := command{Version: 1, ID: "orphan", Type: "sync_orphan_list", Server: server.URL, ServerID: serverID, Workspace: "space", User: "alice", Token: "native-token", StateDir: filepath.Join(private, "state")}
	result := syncEvent(t.Context(), request)
	if result.Type != "sync_orphan_listed" || result.OrphanPage == nil || len(result.OrphanPage.Items) != 0 || requests.Load() != 1 {
		t.Fatal(result, requests.Load())
	}
	request.Type = "sync_preview"
	if result = syncEvent(t.Context(), request); result.Error != "sync_disabled" || requests.Load() != 1 {
		t.Fatal("maintenance enabled synchronization", result, requests.Load())
	}
	request.Type = "sync_orphan_list"
	legacy.Store(true)
	if result = syncEvent(t.Context(), request); result.Error != "sync_disabled" || requests.Load() != 1 {
		t.Fatal("probed unsupported endpoint", result, requests.Load())
	}
	legacy.Store(false)
	request.ServerID = strings.Repeat("f", 64)
	if result = syncEvent(t.Context(), request); result.Error != "sync_binding" || requests.Load() != 1 {
		t.Fatal("sent old IDs to replacement instance", result, requests.Load())
	}
	request.ServerID = serverID
	request.Type = "sync_orphan_retire"
	request.Device = "old-device"
	request.OperationID = strings.Repeat("a", 32)
	request.Confirmation = strings.Repeat("b", 64)
	if result = syncEvent(t.Context(), request); result.Error != "sync_disabled" || requests.Load() != 1 {
		t.Fatal("read capability authorized disposal", result, requests.Load())
	}
}
