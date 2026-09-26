package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/gitx"
	"agentbox/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Real Linux Git crosses the container/host bridge to synthetic TLS and SSH
// providers. No host mounts, real accounts, or external repositories are used.
func TestGitBridgeContainerLive(t *testing.T) {
	image := os.Getenv("AGENTBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set AGENTBOX_DOCKER_TEST_IMAGE to exercise the container transport bridge")
	}
	for _, protocol := range []string{"https", "ssh"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			s, sess := newTestServer(t)
			defer func() {
				cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
				defer done()
				if err := s.Close(cleanup); err != nil {
					t.Error(err)
				}
			}()
			bridgeHost := os.Getenv("AGENTBOX_DOCKER_BRIDGE_HOST")
			if bridgeHost == "" {
				bridgeHost = "host.docker.internal"
			}
			s.cfg.ProxyBridge = config.ProxyBridgeConfig{Bind: "0.0.0.0:0", Host: bridgeHost}
			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			created, err := cli.ContainerCreate(ctx, &container.Config{Image: image, User: "1000:1000", WorkingDir: "/workspace", Cmd: []string{"sleep", "infinity"}}, &container.HostConfig{ExtraHosts: []string{"host.docker.internal:host-gateway"}, SecurityOpt: []string{"no-new-privileges:true"}, Resources: container.Resources{Memory: 256 << 20}}, nil, nil, "")
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
			manager, err := dockerx.New(s.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			s.git = gitx.New(manager, func(context.Context, string) (string, func(), error) { return created.ID, func() {}, nil })
			if err = s.store.Put(sess); err != nil {
				t.Fatal(err)
			}
			if err = s.store.CreateUser(store.User{Name: sess.User, Role: store.RoleUser, PassHash: "fixture"}); err != nil {
				t.Fatal(err)
			}
			// Host directory discovery is synthetic; repository execution is entirely
			// inside the container, so no macOS filesystem semantics enter Git commands.
			if err = os.Mkdir(filepath.Join(s.workspaceDir(sess), ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare", "-q", "-b", "main")
			var conn store.GitConnection
			var repository string
			if protocol == "https" {
				gitCmd(t, remote, "config", "http.receivepack", "true")
				execPath, err := exec.Command("git", "--exec-path").Output()
				if err != nil {
					t.Fatal(err)
				}
				handler := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"), Root: "/", Dir: filepath.Dir(remote), Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(remote), "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "REMOTE_USER=fixture"}}
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					u, p, ok := r.BasicAuth()
					if !ok || u != "fixture" || p != "synthetic-secret" {
						http.Error(w, "denied", 401)
						return
					}
					handler.ServeHTTP(w, r)
				}))
				defer upstream.Close()
				ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))
				raw, _ := json.Marshal(map[string]any{"label": "fixture", "read_only": false, "provider": "gitlab", "base_url": upstream.URL, "username": "fixture", "token": "synthetic-secret", "network": map[string]string{"route": "direct", "ca_pem": ca}})
				w := httptest.NewRecorder()
				s.handleGitConnections(w, accessRequest(sess.User, "POST", "/connections", string(raw)))
				if w.Code != 201 {
					t.Fatal(w.Body.String())
				}
				json.Unmarshal(w.Body.Bytes(), &conn)
				repository = upstream.URL + "/" + filepath.Base(remote)
			} else {
				signer, key := sshFixtureKey(t)
				base, hostKey := startGitSSHFixture(t, remote, signer.PublicKey())
				raw, _ := json.Marshal(map[string]any{"label": "fixture", "read_only": false, "provider": "gitlab", "base_url": base, "username": "git", "auth_type": "ssh", "private_key": key, "host_key": hostKey})
				w := httptest.NewRecorder()
				s.handleGitConnections(w, accessRequest(sess.User, "POST", "/connections", string(raw)))
				if w.Code != 201 {
					t.Fatal(w.Body.String())
				}
				json.Unmarshal(w.Body.Bytes(), &conn)
				repository = base + "/team/repo.git"
			}
			// The container sees only scoped transport tickets, never provider secrets.
			conn, err = s.store.GitConnectionFor(sess.User, conn.ID)
			if err != nil {
				t.Fatal(err)
			}
			repository, err = gitRepositoryURL(repository, conn)
			if err != nil {
				t.Fatal(err)
			}
			transport, closeGrant, err := s.gitTransport(ctx, conn, repository, false, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.runGitNetwork(ctx, sess, s.workspaceDir(sess), "clone", "--no-checkout", "--", transport, "clone")
			closeGrant()
			if err != nil {
				t.Fatalf("container clone: %v", err)
			}
			run := func(args ...string) string {
				t.Helper()
				out, err := s.git.Run(ctx, sess.ID, "", args...)
				if err != nil {
					t.Fatalf("container Git %s: %v", args[0], err)
				}
				return strings.TrimSpace(out)
			}
			run("init", "-q", "-b", "main")
			run("remote", "add", "origin", repository)
			if _, err = manager.ExecCommand(ctx, created.ID, []string{"/bin/sh", "-c", "printf 'fixture\n' > /workspace/hello.txt; printf '/clone/\n' > /workspace/.gitignore"}); err != nil {
				t.Fatal(err)
			}
			run("add", "hello.txt", ".gitignore")
			run("-c", "user.name=Fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "fixture")
			b := store.GitBinding{SessionID: sess.ID, Remote: "origin", URL: repository, ConnectionID: conn.ID}
			if err = s.store.SaveGitBinding(sess.User, b, 0); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.handleGitPushPreview(w, accessRequest(sess.User, "POST", "/push-preview", `{"remote":"origin"}`), sess)
			if w.Code != 200 {
				t.Fatalf("preview: %s", w.Body.String())
			}
			var preview map[string]any
			json.Unmarshal(w.Body.Bytes(), &preview)
			raw, _ := json.Marshal(map[string]any{"remote": "origin", "expected_head": preview["expected_head"], "expected_remote_head": preview["expected_remote_head"], "ref": preview["ref"]})
			w = httptest.NewRecorder()
			s.handleGitPush(w, accessRequest(sess.User, "POST", "/push", string(raw)), sess)
			if w.Code != 200 {
				t.Fatalf("push: %s", w.Body.String())
			}
			expected := run("rev-parse", "HEAD")
			if got, err := exec.Command("git", "-C", remote, "rev-parse", "refs/heads/main").Output(); err != nil || strings.TrimSpace(string(got)) != expected {
				t.Fatal("upstream did not receive commit")
			}
			w = httptest.NewRecorder()
			s.handleGitFetch(w, accessRequest(sess.User, "POST", "/fetch", `{"remote":"origin"}`), sess)
			if w.Code != 200 {
				t.Fatalf("fetch: %s", w.Body.String())
			}
			if got := run("rev-parse", "refs/remotes/origin/main"); got != expected {
				t.Fatal("container did not fetch ref")
			}
			run("config", "branch.main.remote", "origin")
			run("config", "branch.main.merge", "refs/heads/main")
			seed := t.TempDir()
			gitCmd(t, seed, "clone", "-q", remote, ".")
			if err = os.WriteFile(filepath.Join(seed, "from-remote.txt"), []byte("upstream fixture"), 0644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, seed, "add", "from-remote.txt")
			gitCmd(t, seed, "commit", "-qm", "upstream")
			gitCmd(t, seed, "push", "-q", "origin", "main")
			w = httptest.NewRecorder()
			s.handleGitPull(w, accessRequest(sess.User, "POST", "/pull", `{"remote":"origin"}`), sess)
			if w.Code != 200 {
				t.Fatalf("pull: %s", w.Body.String())
			}
			if out, err := manager.ExecCommand(ctx, created.ID, []string{"cat", "/workspace/from-remote.txt"}); err != nil || out != "upstream fixture" {
				t.Fatalf("pull did not update container: %s %v", out, err)
			}
			run("-c", "user.name=Fixture", "-c", "user.email=fixture@localhost", "commit", "--allow-empty", "-qm", "second")
			// Reuse authorization from an actual helper process inside this container.
			if err = os.MkdirAll(s.homeDir(sess), 0700); err != nil {
				t.Fatal(err)
			}
			login := "synthetic-terminal-login"
			if err = s.store.CreateToken(login, sess.User); err != nil {
				t.Fatal(err)
			}
			req := accessRequest(sess.User, "POST", "/terminal", `{"remote":"origin","write":true}`)
			req.Header.Set("Authorization", "Bearer "+login)
			w = httptest.NewRecorder()
			s.handleGitTerminal(w, req, sess)
			if w.Code != 201 {
				t.Fatalf("terminal grant: %s", w.Body.String())
			}
			var grant gitTerminalGrant
			json.Unmarshal(w.Body.Bytes(), &grant)
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			for _, name := range []string{"abox-git", "grant.json"} {
				data, err := os.ReadFile(filepath.Join(s.homeDir(sess), ".agentbox-git", grant.ID, name))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte("synthetic-secret")) || bytes.Contains(data, []byte(login)) {
					t.Fatal("provider/login secret copied")
				}
				if err = tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0700, Uid: 1000, Gid: 1000}); err != nil {
					t.Fatal(err)
				}
				tw.Write(data)
			}
			tw.Close()
			if err = cli.CopyToContainer(ctx, created.ID, "/home/agent", &archive, container.CopyToContainerOptions{}); err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"status", "fetch", "push-preview"} {
				if out, err := manager.ExecCommand(ctx, created.ID, []string{"python3", "-I", "/home/agent/abox-git", action}); err != nil {
					t.Fatalf("terminal %s: %s %v", action, out, err)
				}
			}
			out, err := manager.ExecCommand(ctx, created.ID, []string{"/bin/sh", "-c", "printf 'push\n' | python3 -I /home/agent/abox-git push"})
			if err != nil || !strings.Contains(out, `"pushed": true`) {
				t.Fatalf("terminal push: %s %v", out, err)
			}
			if got, err := exec.Command("git", "-C", remote, "rev-parse", "refs/heads/main").Output(); err != nil || strings.TrimSpace(string(got)) != run("rev-parse", "HEAD") {
				t.Fatal("terminal push not applied")
			}
			s.gitTerminal.mu.Lock()
			active := s.gitTerminal.grants[grant.ID]
			s.gitTerminal.mu.Unlock()
			active.cancel()
			if err = s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			t.Log("container bridge verified:", protocol)
		})
	}
}
