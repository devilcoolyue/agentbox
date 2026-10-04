package syncclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/syncproto"
)

func TestDesktopSyncBindUsesNativeAndServerIdentity(t *testing.T) {
	serverID := syncproto.HashBytes([]byte("sidecar-server"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions/space/sync/manifest" {
			rules, _ := syncproto.ParseRules("")
			m := syncproto.Manifest{Version: 1, RulesHash: rules.Hash(), Entries: map[string]syncproto.Entry{}}
			digest, _ := m.Digest()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Agentbox-Server-ID", serverID)
			_ = json.NewEncoder(w).Encode(syncproto.ManifestResponse{Manifest: m, Digest: digest, Project: "project", Revision: 1, ProjectPath: "."})
			return
		}
		if r.URL.Path != "/api/clients/capabilities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Agentbox-Server-ID", serverID)
		_ = json.NewEncoder(w).Encode(syncproto.ServerIdentity{Version: 1, ServerID: serverID, User: "alice", Features: map[string]int{"sync": 1}})
	}))
	defer server.Close()
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateBase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(privateBase, "state")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	probeState, err := OpenState(private)
	if err != nil {
		t.Fatalf("state setup: %v", err)
	}
	probeState.Close()
	input := map[string]any{"version": 1, "id": "bind", "type": "sync_bind", "server": server.URL, "token": "private-token", "user": "alice", "state_dir": private, "directory": local, "binding": map[string]any{"user": "alice", "workspace": "space", "project": "project", "project_path": "."}}
	encoded, _ := json.Marshal(input)
	var request command
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	event := syncEvent(t.Context(), request)
	if event.Type != "sync_bound" || event.Binding == nil {
		t.Fatalf("event: %+v", event)
	}
	if _, err := os.Stat(filepath.Join(private, "sync.db")); err != nil {
		t.Fatal("sync state was not persisted", err)
	}
}

func TestDesktopHandshakeCommandsAndEOF(t *testing.T) {
	var out bytes.Buffer
	input := strings.NewReader("{\"version\":1,\"id\":\"p\",\"type\":\"ping\"}\n{\"version\":99,\"id\":\"secret\",\"type\":\"ping\"}\n{\"version\":1,\"id\":\"x\",\"type\":\"sync\"}\n")
	if err := RunDesktop(input, &out); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	var ready struct {
		Type         string
		Version      int
		Capabilities []string
	}
	if err := decoder.Decode(&ready); err != nil || ready.Type != "ready" || ready.Version != 1 || len(ready.Capabilities) != 2 || ready.Capabilities[0] != "local_inspect_v1" {
		t.Fatalf("bad ready: %+v, %v", ready, err)
	}
	for _, want := range []event{{Version: 1, ID: "p", Type: "pong"}, {Version: 1, Type: "error", Error: "unsupported_version"}, {Version: 1, ID: "x", Type: "error", Error: "unsupported_command"}} {
		var got event
		if err := decoder.Decode(&got); err != nil || got.Type != want.Type || got.ID != want.ID || got.Error != want.Error {
			t.Fatalf("got %+v, want %+v, err %v", got, want, err)
		}
	}
}

func TestDesktopParentPipeClosureExits(t *testing.T) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- RunDesktop(reader, io.Discard) }()
	writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sidecar remained alive after parent EOF")
	}
}

func TestDesktopLimitsAndShutdown(t *testing.T) {
	var out bytes.Buffer
	if err := RunDesktop(strings.NewReader(strings.Repeat("x", maxCommand+1)), &out); err == nil {
		t.Fatal("unbounded input accepted")
	}
	out.Reset()
	if err := RunDesktop(strings.NewReader("{\"version\":1,\"id\":\"s\",\"type\":\"shutdown\"}\n{\"version\":1,\"id\":\"p\",\"type\":\"ping\"}\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"stopped"`) || strings.Contains(out.String(), `"pong"`) {
		t.Fatal(out.String())
	}
}

func TestDesktopEOFAndShutdownCancelActiveSyncRequest(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(canceled)
			}))
			defer server.Close()
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			done := make(chan error, 1)
			go func() { done <- RunDesktop(input, io.Discard) }()
			request := command{Version: 1, ID: "preview", Type: "sync_preview", Server: server.URL, User: "alice", Token: "private-token", StateDir: "unused-until-auth", BindingID: strings.Repeat("a", 32)}
			if err := json.NewEncoder(writer).Encode(request); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("sync did not start")
			}
			if shutdown {
				if err := json.NewEncoder(writer).Encode(command{Version: 1, ID: "stop", Type: "shutdown"}); err != nil {
					t.Fatal(err)
				}
			} else {
				writer.Close()
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("network request survived parent cancellation")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("sidecar did not exit")
			}
		})
	}
}
func TestDesktopRejectsUserMismatchBeforeOpeningState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.ServerIdentity{Version: 1, ServerID: syncproto.HashBytes([]byte("server")), User: "bob", Features: map[string]int{"sync": 1}})
	}))
	defer server.Close()
	request := command{Version: 1, ID: "list", Type: "sync_list", Server: server.URL, User: "alice", Token: "private-token", StateDir: filepath.Join(t.TempDir(), "must-not-exist")}
	result := syncEvent(t.Context(), request)
	if result.Error != "sync_identity" || result.Bindings != nil {
		t.Fatal("wrong user accepted", result)
	}
	if _, err := os.Stat(request.StateDir); !os.IsNotExist(err) {
		t.Fatal("mismatch touched state", err)
	}
}
