package dockerx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

func TestCLIProbeIsolationAndCleanup(t *testing.T) {
	for _, failure := range []string{"behavior", "start", "cancel", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			id := "sha256:" + strings.Repeat("a", 64)
			created, removed := false, false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Header().Set("API-Version", "1.45")
					_, _ = io.WriteString(w, "OK")
				case strings.Contains(r.URL.Path, "/images/"):
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Env": []string{"INHERITED_TOKEN=synthetic-secret", "NODE_OPTIONS=--require=/bad"}}})
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					created = true
					var body struct {
						container.Config
						HostConfig container.HostConfig
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					host := body.HostConfig
					if body.Image != id || body.User != "1000:1000" || !body.NetworkDisabled || host.NetworkMode != "none" || !host.ReadonlyRootfs || len(host.Binds) != 0 || len(host.Mounts) != 0 || host.Privileged {
						t.Error("probe escaped isolation")
					}
					if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" || host.SecurityOpt[0] != "no-new-privileges" || host.Memory <= 0 || host.PidsLimit == nil || *host.PidsLimit > 128 || host.Tmpfs["/tmp"] == "" {
						t.Error("probe limits missing")
					}
					if strings.Join(body.Entrypoint, " ") != "/usr/local/bin/node" {
						t.Error("image entrypoint inherited")
					}
					for _, env := range body.Env {
						if strings.Contains(env, "synthetic-secret") || strings.Contains(env, "--require=") {
							t.Error("inherited environment")
						}
					}
					_, _ = io.WriteString(w, `{"Id":"owned-probe"}`)
				case strings.HasSuffix(r.URL.Path, "/start"):
					if failure == "start" {
						w.WriteHeader(500)
						_, _ = io.WriteString(w, `{"message":"synthetic failure"}`)
					} else {
						w.WriteHeader(204)
					}
				case strings.HasSuffix(r.URL.Path, "/logs"):
					if failure == "cancel" {
						cancel()
						<-r.Context().Done()
						return
					}
					_, _ = io.WriteString(stdcopy.NewStdWriter(w, stdcopy.Stdout), `{"version":1,"failed":"claude_turn"}`)
				case strings.HasSuffix(r.URL.Path, "/wait"):
					_, _ = io.WriteString(w, `{"StatusCode":1}`)
				case r.Method == "DELETE":
					removed = true
					if r.URL.Path != "/v1.45/containers/owned-probe" || r.URL.Query().Get("force") != "1" || r.URL.Query().Get("v") != "1" {
						t.Error("cleanup not limited to owned container/volumes", r.URL.String())
					}
					if failure == "cleanup" {
						w.WriteHeader(500)
						_, _ = io.WriteString(w, `{"message":"synthetic failure"}`)
					} else {
						w.WriteHeader(204)
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer engine.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(engine.URL, "http://"))
			t.Setenv("DOCKER_API_VERSION", "1.45")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			m, err := New(&config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			report, err := m.ValidateCLIImage(ctx, id)
			if err == nil || report.Passed(id) || !created || !removed {
				t.Fatalf("missing failure/cleanup: %+v, %v", report, err)
			}
			if failure == "cleanup" && report.Failure != "probe_cleanup_failed" {
				t.Fatal(report)
			}
			if failure == "behavior" && report.Stage != "claude_turn" {
				t.Fatal(report)
			}
		})
	}
}
