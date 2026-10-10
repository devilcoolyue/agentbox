package dockerx

import (
	"agentbox/internal/config"
	"agentbox/internal/imageupdate"
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/docker/docker/pkg/stdcopy"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/image"
)

func TestFlatArchivePreservesConfigAndLayerDigest(t *testing.T) {
	// Include fields outside Dockerfile/import's convenient subset. Unknown
	// configuration is preserved too; nothing is reconstructed from CLI flags.
	raw := json.RawMessage(`{"Env":["X=a=b","PATH=/custom"],"User":"123:456","WorkingDir":"/custom","Entrypoint":["/entry"],"Cmd":["arg"],"Shell":["/bin/bash","-c"],"Healthcheck":{"Test":["CMD","true"],"StartInterval":123},"StopSignal":"SIGQUIT","Labels":{"custom":"keep"},"FutureField":{"x":1}}`)
	source := "sha256:" + strings.Repeat("a", 64)
	cfg, err := flatConfig(raw, source)
	if err != nil {
		t.Fatal(err)
	}
	layer := []byte("synthetic layer bytes")
	var output bytes.Buffer
	id, err := writeFlatArchive(&output, bytes.NewReader(layer), int64(len(layer)), image.InspectResponse{Architecture: "arm64", Variant: "v8", Os: "linux"}, cfg, "test:flat")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	tr := tar.NewReader(&output)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		entries[h.Name], err = io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
	}
	data := entries[strings.TrimPrefix(id, "sha256:")+".json"]
	hash := sha256.Sum256(data)
	if id != "sha256:"+hex.EncodeToString(hash[:]) {
		t.Fatal("config digest mismatch")
	}
	var loaded struct {
		Config json.RawMessage
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		}
	}
	if err = json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	layerHash := sha256.Sum256(layer)
	if len(loaded.RootFS.DiffIDs) != 1 || loaded.RootFS.DiffIDs[0] != "sha256:"+hex.EncodeToString(layerHash[:]) {
		t.Fatal("wrong rootfs")
	}
	if !sameJSON(cfg, loaded.Config) || !bytes.Equal(layer, entries["rootfs/layer.tar"]) {
		t.Fatal("lost metadata or rootfs")
	}
	for _, bad := range []int64{int64(len(layer) - 1), int64(len(layer) + 1)} {
		if _, err = writeFlatArchive(io.Discard, bytes.NewReader(layer), bad, image.InspectResponse{}, cfg, "test:flat"); err == nil {
			t.Fatal("accepted short/long export")
		}
	}
}

func TestFlattenRefusesUnsafeImagesAndInsufficientSpace(t *testing.T) {
	for _, kind := range []string{"volume", "onbuild", "windows", "space", "space-cleanup"} {
		t.Run(kind, func(t *testing.T) {
			id := "sha256:" + strings.Repeat("a", 64)
			exports := 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Write([]byte("OK"))
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/containers/agentbox-cli-flatten-"):
					w.WriteHeader(404)
					w.Write([]byte(`{"message":"missing"}`))
				case strings.Contains(r.URL.Path, "/images/agentbox-agent:base-"):
					w.WriteHeader(404)
					w.Write([]byte(`{"message":"missing"}`))
				case strings.Contains(r.URL.Path, "/images/"):
					cfg := map[string]any{"Labels": map[string]string{}}
					system := "linux"
					if kind == "volume" {
						cfg["Volumes"] = map[string]any{"/data": map[string]any{}}
					}
					if kind == "onbuild" {
						cfg["OnBuild"] = []string{"RUN touch /bad"}
					}
					if kind == "windows" {
						system = "windows"
					}
					json.NewEncoder(w).Encode(map[string]any{"Id": id, "Os": system, "Architecture": "amd64", "Config": cfg})
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					var cfg struct{ Labels map[string]string }
					json.NewDecoder(r.Body).Decode(&cfg)
					if cfg.Labels["agentbox.cli-space"] != "1" {
						t.Error("created export container before space gate")
					}
					w.Write([]byte(`{"Id":"space-probe"}`))
				case strings.HasSuffix(r.URL.Path, "/start"):
					w.WriteHeader(204)
				case strings.HasSuffix(r.URL.Path, "/logs"):
					stdcopy.NewStdWriter(w, stdcopy.Stdout).Write([]byte("1024\n"))
				case strings.HasSuffix(r.URL.Path, "/wait"):
					w.Write([]byte(`{"StatusCode":0}`))
				case r.Method == "DELETE":
					if kind == "space-cleanup" {
						w.WriteHeader(500)
						w.Write([]byte(`{"message":"cleanup failed"}`))
					} else {
						w.WriteHeader(204)
					}
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/export"):
					exports++
					w.WriteHeader(500)
				default:
					t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
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
			if _, err = m.FlattenCLIImage(t.Context(), imageupdate.Image{ID: id}, func(string) {}); err == nil || exports != 0 {
				t.Fatal("unsafe flatten", err, exports)
			}
		})
	}
}

func TestFlattenResumeCleansOnlyItsUnstartedContainer(t *testing.T) {
	for _, kind := range []string{"resume", "cleanup-failed", "foreign", "running", "mounted"} {
		t.Run(kind, func(t *testing.T) {
			source := "sha256:" + strings.Repeat("a", 64)
			flat := "sha256:" + strings.Repeat("b", 64)
			removed := false
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Write([]byte("OK"))
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/containers/"):
					src := source
					state := "created"
					mounts := []any{}
					if kind == "foreign" {
						src = "another-source"
					}
					if kind == "running" {
						state = "running"
					}
					if kind == "mounted" {
						mounts = append(mounts, map[string]string{"Destination": "/workspace"})
					}
					json.NewEncoder(w).Encode(map[string]any{"Id": "owned-export", "Image": source, "Config": map[string]any{"Labels": map[string]string{flatSourceLabel: src}}, "State": map[string]string{"Status": state}, "Mounts": mounts})
				case r.Method == "DELETE":
					removed = true
					if r.URL.Query().Get("force") == "1" || !strings.HasSuffix(r.URL.Path, "/containers/owned-export") {
						t.Error("unsafe orphan cleanup")
					}
					if kind == "cleanup-failed" {
						w.WriteHeader(500)
						w.Write([]byte(`{"message":"cleanup failed"}`))
					} else {
						w.WriteHeader(204)
					}
				case strings.Contains(r.URL.Path, "/images/"):
					id := source
					labels := map[string]string{}
					if !strings.HasSuffix(r.URL.Path, source+"/json") {
						id = flat
						labels[flatSchemaLabel] = "1"
						labels[flatSourceLabel] = source
						if !removed {
							t.Error("reused staged base before orphan cleanup")
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"Id": id, "Os": "linux", "Architecture": "amd64", "Config": map[string]any{"Labels": labels}, "RootFS": map[string]any{"Type": "layers", "Layers": []string{"sha256:layer"}}})
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
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
			got, err := m.FlattenCLIImage(t.Context(), imageupdate.Image{ID: source}, func(string) {})
			if kind == "resume" {
				if err != nil || got.ID != flat || !removed {
					t.Fatal(got, err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe resume")
				}
				if kind != "cleanup-failed" && removed {
					t.Fatal("removed foreign/active/mounted container")
				}
			}
		})
	}
}

func TestFlattenContainerdIDAndImportedDigest(t *testing.T) {
	for _, kind := range []string{"manifest-id", "wrong-layer", "wrong-config"} {
		t.Run(kind, func(t *testing.T) {
			source := "sha256:" + strings.Repeat("a", 64)
			actual := "sha256:" + strings.Repeat("b", 64)
			sourceConfig := json.RawMessage(`{"Labels":{"agentbox.claude-code":"2.1.280","agentbox.codex":"0.156.1"}}`)
			expected, _ := flatConfig(sourceConfig, source)
			layer := []byte("synthetic rootfs")
			sum := sha256.Sum256(layer)
			diff := "sha256:" + hex.EncodeToString(sum[:])
			imported, removed := false, false
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Write([]byte("OK"))
				case strings.HasSuffix(r.URL.Path, "/images/load"):
					io.Copy(io.Discard, r.Body)
					imported = true
					w.Write([]byte(`{"stream":"Loaded image"}`))
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/images/"):
					if strings.Contains(r.URL.Path, "agentbox-agent:base-") && !imported {
						w.WriteHeader(404)
						w.Write([]byte(`{"message":"missing"}`))
						return
					}
					cfg := sourceConfig
					id := source
					layers := []string{"sha256:old-layer"}
					if !strings.HasSuffix(r.URL.Path, source+"/json") {
						id = actual
						cfg = expected
						layers = []string{diff}
						if kind == "wrong-layer" {
							layers = []string{"sha256:unexpected"}
						}
						if kind == "wrong-config" {
							cfg = sourceConfig
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"Id": id, "Os": "linux", "Architecture": "amd64", "Config": cfg, "RootFS": map[string]any{"Type": "layers", "Layers": layers}})
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/containers/agentbox-cli-flatten-"):
					w.WriteHeader(404)
					w.Write([]byte(`{"message":"missing"}`))
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					var cfg struct{ Labels map[string]string }
					json.NewDecoder(r.Body).Decode(&cfg)
					id := "export"
					if cfg.Labels["agentbox.cli-space"] == "1" {
						id = "space"
					}
					json.NewEncoder(w).Encode(map[string]string{"Id": id})
				case strings.HasSuffix(r.URL.Path, "/start"):
					w.WriteHeader(204)
				case strings.HasSuffix(r.URL.Path, "/logs"):
					stdcopy.NewStdWriter(w, stdcopy.Stdout).Write([]byte("999999999999\n"))
				case strings.HasSuffix(r.URL.Path, "/wait"):
					w.Write([]byte(`{"StatusCode":0}`))
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/export"):
					w.Write(layer)
				case r.Method == "DELETE":
					if strings.HasSuffix(r.URL.Path, "/export") {
						removed = true
					}
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
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
			got, err := m.FlattenCLIImage(t.Context(), imageupdate.Image{ID: source}, func(string) {})
			if !removed || !imported {
				t.Fatal("missing load or cleanup")
			}
			if kind == "manifest-id" {
				if err != nil || got.ID != actual {
					t.Fatal("assumed config digest is daemon ID", got, err)
				}
			} else if err == nil {
				t.Fatal("accepted changed imported bytes")
			}
		})
	}
}
