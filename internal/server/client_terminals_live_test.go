package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"agentbox/internal/workspace"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

// Exercises Go handlers + real Linux Docker PTY/tmux. Host project paths are
// synthetic mirrors of the fixture image (no user mounts, network or models).
func TestClientTerminalsContainerLive(t *testing.T) {
	image := os.Getenv("AGENTBOX_CLIENT_TEST_IMAGE")
	if image == "" {
		t.Skip("set AGENTBOX_CLIENT_TEST_IMAGE to the client.Dockerfile fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s, sess, handler := clientTestServer(t)
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	created, err := cli.ContainerCreate(ctx, &container.Config{Image: image, User: "1000:1000", WorkingDir: "/workspace", Cmd: []string{"sleep", "infinity"}}, &container.HostConfig{NetworkMode: "none", SecurityOpt: []string{"no-new-privileges:true"}, Resources: container.Resources{Memory: 256 << 20}}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{Force: true}); err != nil {
			t.Error(err)
		}
	}()
	if err = cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	s.dock, err = dockerx.New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.dock.Close()
	sess.ContainerID = created.ID
	sess.Status = store.StatusRunning
	if err = s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	// Skip optional network/MCP hooks in this isolated terminal fixture. Use the
	// real workspace lock/account/activity services and actual Docker operations.
	s.workspaceOnce.Do(func() { s.workspace = workspace.New(s.cfg, s.store, clientLiveRuntime{s.dock}, s.sessionAccount, nil) })
	s.upgrader = websocket.Upgrader{CheckOrigin: sameHostOrigin}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := s.Close(cleanup); err != nil {
			t.Error(err)
		}
	}()
	newTerminal := func(dir string) store.ClientTerminal {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(s.workspaceDir(sess), dir), 0755); err != nil {
			t.Fatal(err)
		}
		p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: dir, Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		terminal, err := s.store.CreateClientTerminal(sess.ID, p.ID, "shell")
		if err != nil {
			t.Fatal(err)
		}
		return terminal
	}
	a, b := newTerminal("project-a"), newTerminal("project-b")
	dial := func(path string) *websocket.Conn {
		t.Helper()
		header := http.Header{"Authorization": []string{"Bearer client-fixture-alice"}}
		conn, response, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, header)
		if err != nil {
			status := 0
			if response != nil {
				status = response.StatusCode
				response.Body.Close()
			}
			t.Fatalf("terminal dial: %v status=%d", err, status)
		}
		return conn
	}
	endpoint := func(id string) string { return "/api/sessions/s1/client-terminals/" + id + "/stream" }
	expect := func(conn *websocket.Conn, command, want string) {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte(command+"\n")); err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		for {
			_, bytes, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("terminal wanted %q: %v output=%q", want, err, output.String())
			}
			output.Write(bytes)
			if strings.Contains(output.String(), want) {
				break
			}
			if output.Len() > 1<<20 {
				t.Fatal("unexpected terminal output overflow")
			}
		}
	}
	ca := dial(endpoint(a.ID))
	defer ca.Close()
	cb := dial(endpoint(b.ID))
	defer cb.Close()
	legacy := dial("/api/sessions/s1/term")
	defer legacy.Close()
	// Split the output marker so shell echo cannot satisfy the assertion.
	expect(ca, "export AB_TEST=first; printf 'READY_%s\\n' \"$PWD\"", "READY_/workspace/project-a")
	expect(cb, "printf 'SECOND_%s_%s\\n' \"$PWD\" \"${AB_TEST-unset}\"", "SECOND_/workspace/project-b_unset")
	expect(legacy, "printf 'LEGACY_%s\\n' \"${AB_TEST-unset}\"", "LEGACY_unset")
	ca.Close()
	ca = dial(endpoint(a.ID))
	defer ca.Close()
	expect(ca, "printf 'KEPT_%s\\n' \"$AB_TEST\"", "KEPT_first")
	resize, _ := json.Marshal(map[string]any{"type": "resize", "cols": 100, "rows": 35})
	if err = ca.WriteMessage(websocket.TextMessage, resize); err != nil {
		t.Fatal(err)
	}
	expect(ca, "printf 'SIZE_'; stty size", "SIZE_34 100")
	w := clientRequest(handler, "alice", "DELETE", "/api/sessions/s1/client-terminals/"+a.ID, "")
	if w.Code != 200 {
		t.Fatalf("terminate: %d %s", w.Code, w.Body.String())
	}
	if _, err = s.store.ClientTerminal(sess.ID, a.ID); err == nil {
		t.Fatal("terminal record remained after stop")
	}
	_, err = s.dock.ExecCommand(ctx, sess.ContainerID, []string{"tmux", "-L", clientTerminalSocket(a.ID), "has-session", "-t", "=work"})
	if err == nil {
		t.Fatal("terminated tmux still exists")
	}
	expect(cb, "printf 'OTHER_%s\\n' alive", "OTHER_alive")
	expect(legacy, "printf 'MAIN_%s\\n' alive", "MAIN_alive")
	// Revocation reaches the already-connected project stream before forwarding input.
	// sessionAccount intentionally uses the attached snapshot, so revoke the
	// shared account itself rather than only mutating the workspace binding.
	if _, err = s.cfg.UpdateAccount("shared", config.AccountPatch{Access: &config.AccountAccess{Mode: "admin"}}); err != nil {
		t.Fatal(err)
	}
	if err = cb.WriteMessage(websocket.BinaryMessage, []byte("echo must-not-run\n")); err != nil {
		t.Fatal(err)
	}
	cb.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err = cb.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, closeAccountAccess) {
				t.Fatalf("revocation close: %v", err)
			}
			break
		}
	}
}

// The fixture has no shared bind mount: tell Start it is already initialized,
// while preserving real Running/Stop/Remove and UseRunning checks.
type clientLiveRuntime struct{ *dockerx.Manager }

func (clientLiveRuntime) RunningWithMount(context.Context, string, string) bool { return true }
