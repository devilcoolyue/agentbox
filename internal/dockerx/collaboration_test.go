package dockerx

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func TestCollaborationContainerProfileAndFailureCleanup(t *testing.T) {
	for _, scenario := range []string{"coordinator", "executor", "bad-mount", "start-failure", "collision", "running", "foreign"} {
		t.Run(scenario, func(t *testing.T) {
			p := CollaborationProfile{RunID: strings.Repeat("a", 32), ParticipantID: strings.Repeat("b", 32), Role: collaboration.Coordinator, ImageID: "sha256:" + strings.Repeat("c", 64), Workspace: "/fixture/work", Home: "/fixture/home", Scratch: "/fixture/scratch", Inputs: "/fixture/inputs"}
			if scenario == "executor" {
				p.Role = collaboration.Executor
			}
			sess := store.Session{ID: p.ParticipantID, User: "alice", Agent: "codex", AccountID: "account"}
			if scenario == "running" || scenario == "foreign" {
				sess.ContainerID = "old"
			}
			var cc container.Config
			var hc container.HostConfig
			creates, removes := 0, 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Header().Set("API-Version", "1.45")
					io.WriteString(w, "OK")
				case strings.Contains(r.URL.Path, "/images/"):
					json.NewEncoder(w).Encode(map[string]any{"Id": p.ImageID, "Os": "linux", "Config": map[string]any{"Env": []string{"INHERITED_TOKEN=secret-fixture", "NODE_OPTIONS=--require=/bad", "PATH=/usr/bin"}}})
				case strings.HasSuffix(r.URL.Path, "/old/json"):
					labels := map[string]string{"agentbox.session": sess.ID, "agentbox.user": sess.User, "agentbox.collaboration": p.RunID}
					if scenario == "foreign" {
						labels["agentbox.user"] = "bob"
					}
					json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": scenario == "running"}})
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					creates++
					if scenario == "collision" {
						w.WriteHeader(409)
						io.WriteString(w, `{"message":"name in use"}`)
						return
					}
					var body struct {
						container.Config
						HostConfig container.HostConfig
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					cc, hc = body.Config, body.HostConfig
					if len(hc.Binds) != 0 || len(hc.Mounts) != 4 || cc.User != "1000:1000" || hc.Privileged || len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
						t.Error("container escaped mount or user limits")
					}
					for _, v := range cc.Env {
						if strings.Contains(v, "secret-fixture") || strings.Contains(v, "--require") {
							t.Error("image secrets/options inherited")
						}
					}
					if cc.Image != p.ImageID || cc.Entrypoint[0] != "/bin/sleep" || cc.Healthcheck.Test[0] != "NONE" {
						t.Error("image execution configuration inherited")
					}
					io.WriteString(w, `{"Id":"owned"}`)
				case strings.HasSuffix(r.URL.Path, "/start"):
					if scenario == "start-failure" {
						w.WriteHeader(500)
						io.WriteString(w, `{"message":"synthetic"}`)
					} else {
						w.WriteHeader(204)
					}
				case strings.HasSuffix(r.URL.Path, "/owned/json"):
					var mounts []container.MountPoint
					for _, m := range hc.Mounts {
						mounts = append(mounts, container.MountPoint{Type: mount.TypeBind, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly})
					}
					if scenario == "bad-mount" {
						mounts[0].RW = true
					}
					json.NewEncoder(w).Encode(map[string]any{"Config": cc, "Image": p.ImageID, "Mounts": mounts})
				case r.Method == http.MethodDelete:
					removes++
					if !strings.HasSuffix(r.URL.Path, "/owned") {
						t.Error("removed foreign/colliding container")
					}
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer engine.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(engine.URL, "http://"))
			t.Setenv("DOCKER_API_VERSION", "1.45")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			m, err := New(&config.Config{Container: config.ContainerLimits{MemoryMB: 256, CPUs: 1, PidsLimit: 64, Network: "none"}})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			id, err := m.EnsureCollaborationRunning(t.Context(), sess, config.Account{ID: "account", Type: "codex"}, p)
			success := scenario == "coordinator" || scenario == "executor"
			if (err == nil) != success {
				t.Fatal(id, err)
			}
			if success {
				if id != "owned" || hc.Mounts[0].ReadOnly != (scenario == "coordinator") || removes != 0 {
					t.Fatal("bad role permissions", id, hc.Mounts)
				}
			}
			if (scenario == "bad-mount" || scenario == "start-failure") && removes != 1 {
				t.Fatal("failed container leaked")
			}
			if (scenario == "running" || scenario == "foreign") && (creates != 0 || removes != 0) {
				t.Fatal("running or foreign container mutated")
			}
			if scenario == "collision" && removes != 0 {
				t.Fatal("name collision removed another container")
			}
		})
	}
}
