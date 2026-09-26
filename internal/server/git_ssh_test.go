package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"golang.org/x/crypto/ssh"
)

func sshFixtureKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
}

// Remote SSH fixture runs real Git only in isolated synthetic temp repositories.
// Production never invokes a host Git process.
func startGitSSHFixture(t *testing.T, repo string, userKey ssh.PublicKey) (string, string) {
	t.Helper()
	host, _ := sshFixtureKey(t)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "git" || string(key.Marshal()) != string(userKey.Marshal()) {
			return nil, os.ErrPermission
		}
		return &ssh.Permissions{}, nil
	}}
	cfg.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				server, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for request := range channels {
					if request.ChannelType() != "session" {
						request.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					channel, requests, err := request.Accept()
					if err != nil {
						return
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer channel.Close()
						protocol := ""
						for request := range requests {
							if request.Type == "env" {
								var env struct{ Name, Value string }
								_ = ssh.Unmarshal(request.Payload, &env)
								if env.Name == "GIT_PROTOCOL" {
									protocol = env.Value
									request.Reply(true, nil)
								} else {
									request.Reply(false, nil)
								}
								continue
							}
							if request.Type != "exec" {
								request.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							_ = ssh.Unmarshal(request.Payload, &payload)
							service := ""
							for _, allowed := range []string{"git-upload-pack", "git-receive-pack"} {
								if payload.Command == allowed+" 'team/repo.git'" {
									service = allowed
								}
							}
							if service == "" {
								request.Reply(false, nil)
								return
							}
							request.Reply(true, nil)
							cmd := exec.CommandContext(ctx, service, repo)
							cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_PROTOCOL="+protocol)
							cmd.Stdin = channel
							cmd.Stdout = channel
							cmd.Stderr = channel.Stderr()
							err := cmd.Run()
							code := uint32(0)
							if err != nil {
								code = 1
							}
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
							return
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); <-done; wg.Wait() })
	return "ssh://" + listener.Addr().String(), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host.PublicKey())))
}
func TestSSHCloneFetchPushAndHostPin(t *testing.T) {
	s, sess := newGitTestServer(t)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateUser(store.User{Name: sess.User, Role: store.RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	s.cfg.ProxyBridge = config.ProxyBridgeConfig{Bind: "127.0.0.1:0", Host: "127.0.0.1"}
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare", "-q", "-b", "main")
	user, key := sshFixtureKey(t)
	base, hostKey := startGitSSHFixture(t, remote, user.PublicKey())
	payload, _ := json.Marshal(map[string]any{"label": "Company SSH", "provider": "gitlab", "base_url": base, "username": "git", "auth_type": "ssh", "private_key": key, "host_key": hostKey, "read_only": false})
	w := httptest.NewRecorder()
	s.handleGitConnections(w, accessRequest(sess.User, "POST", "/git/connections", string(payload)))
	if w.Code != 201 {
		t.Fatalf("create SSH: %d %s", w.Code, w.Body.String())
	}
	var c store.GitConnection
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), "PRIVATE KEY") {
		t.Fatal("SSH key leaked")
	}
	ws := s.workspaceDir(sess)
	gitCmd(t, ws, "init", "-q", "-b", "main")
	gitCmd(t, ws, "remote", "add", "origin", base+"/team/repo.git")
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("ssh fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ws, "add", "-A")
	gitCmd(t, ws, "commit", "-q", "-m", "first")
	binding, _ := json.Marshal(map[string]any{"repo": "", "remote": "origin", "connection_id": c.ID, "revision": 0})
	w = httptest.NewRecorder()
	s.handleGitBindings(w, accessRequest(sess.User, "PUT", "/bindings", string(binding)), sess)
	if w.Code != 200 {
		t.Fatalf("bind: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleGitPushPreview(w, accessRequest(sess.User, "POST", "/preview", `{"repo":"","remote":"origin"}`), sess)
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var preview struct {
		Ref        string `json:"ref"`
		Head       string `json:"expected_head"`
		RemoteHead string `json:"expected_remote_head"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	push, _ := json.Marshal(map[string]string{"repo": "", "remote": "origin", "ref": preview.Ref, "expected_head": preview.Head, "expected_remote_head": preview.RemoteHead})
	w = httptest.NewRecorder()
	s.handleGitPush(w, accessRequest(sess.User, "POST", "/push", string(push)), sess)
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
	output, err := exec.Command("git", "-C", remote, "rev-parse", "main").Output()
	if err != nil || strings.TrimSpace(string(output)) != preview.Head {
		t.Fatalf("push refs: %s %v", output, err)
	}
	w = httptest.NewRecorder()
	s.handleGitFetch(w, accessRequest(sess.User, "POST", "/fetch", `{"repo":"","remote":"origin"}`), sess)
	if w.Code != 200 {
		t.Fatalf("fetch: %s", w.Body.String())
	}
	clone, _ := json.Marshal(map[string]string{"connection_id": c.ID, "url": base + "/team/repo.git", "directory": "ssh-clone"})
	w = httptest.NewRecorder()
	s.handleGitClone(w, accessRequest(sess.User, "POST", "/clone", string(clone)), sess)
	if w.Code != 201 {
		t.Fatalf("clone: %s", w.Body.String())
	}
	if contents, err := os.ReadFile(filepath.Join(ws, "ssh-clone", "hello.txt")); err != nil || string(contents) != "ssh fixture" {
		t.Fatalf("clone checkout: %v", err)
	}
	stored, err := s.store.GitConnection(sess.User, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	probe, _ := json.Marshal(map[string]string{"url": base + "/team/repo.git"})
	r := accessRequest(sess.User, "POST", "/test", string(probe))
	r.SetPathValue("connection", c.ID)
	w = httptest.NewRecorder()
	s.handleGitConnectionTest(w, r)
	if w.Code != 200 {
		t.Fatalf("SSH test: %s", w.Body.String())
	}
	raw, err := s.gitVault().Open(stored.Secret, stored.AssociatedData())
	if err != nil {
		t.Fatal(err)
	}
	var cred map[string]string
	json.Unmarshal(raw, &cred)
	wrong, _ := sshFixtureKey(t)
	cred["host_key"] = string(ssh.MarshalAuthorizedKey(wrong.PublicKey()))
	raw, _ = json.Marshal(cred)
	stored.Secret, err = s.gitVault().Seal(raw, stored.AssociatedData(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.SaveGitConnection(stored, stored.Revision); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleGitConnectionTest(w, r)
	if w.Code == 200 {
		t.Fatal("wrong SSH host pin accepted")
	}
}
