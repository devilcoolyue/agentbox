package dockerx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"agentbox/internal/config"
)

func TestImageGCUsesExactIDsAndStopsAtProtectedParent(t *testing.T) {
	for _, kind := range []string{"owned", "foreign", "stopped", "protected", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			var deleted []string
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Write([]byte("OK"))
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/images/"):
					id := "owned"
					parent := "intermediate"
					tags := []string{"agentbox-agent:cli-123"}
					if strings.Contains(r.URL.Path, "/intermediate/") {
						id = "intermediate"
						parent = "base"
						tags = nil
					}
					if kind == "foreign" {
						tags = append(tags, "custom:keep")
					}
					json.NewEncoder(w).Encode(map[string]any{"Id": id, "Parent": parent, "RepoTags": tags})
				case strings.HasSuffix(r.URL.Path, "/containers/json"):
					if r.URL.Query().Get("all") != "1" {
						t.Error("missed stopped containers")
					}
					if kind == "stopped" {
						w.Write([]byte(`[{"Id":"user-container","ImageID":"owned"}]`))
					} else {
						w.Write([]byte(`[]`))
					}
				case r.Method == "DELETE":
					if r.URL.Query().Get("force") == "1" || r.URL.Query().Get("noprune") != "1" {
						t.Error("unsafe deletion options")
					}
					deleted = append(deleted, r.URL.Path)
					if kind == "conflict" {
						w.WriteHeader(409)
						w.Write([]byte(`{"message":"referenced"}`))
					} else {
						w.Write([]byte(`[]`))
					}
				default:
					t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
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
			protected := map[string]bool{"base": true}
			if kind == "protected" {
				protected["owned"] = true
			}
			removed, err := m.RemoveCLIImage(t.Context(), "owned", protected)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "owned" {
				if !removed || !reflect.DeepEqual(deleted, []string{"/v1.45/images/owned", "/v1.45/images/intermediate"}) {
					t.Fatal(removed, deleted)
				}
			} else if removed || (kind != "conflict" && len(deleted) != 0) {
				t.Fatal("removed protected resource", deleted)
			}
		})
	}
}

func TestRuntimeComparisonAllowsParentMetadataNotExecutionChanges(t *testing.T) {
	a := json.RawMessage(`{"Image":"old-parent","User":"root","Env":["SECRET=synthetic"],"Cmd":["sleep","infinity"],"Labels":{"agentbox.claude-code":"1.0.0","custom":"keep"}}`)
	b := json.RawMessage(`{"Image":"fixed-parent","User":"root","Env":["SECRET=synthetic"],"Cmd":["sleep","infinity"],"Labels":{"agentbox.claude-code":"2.0.0","custom":"keep"}}`)
	if !sameJSON(runtimeConfig(a), runtimeConfig(b)) {
		t.Fatal("parent metadata blocked fixed-base builds")
	}
	for _, change := range []string{strings.Replace(string(b), "synthetic", "changed", 1), strings.Replace(string(b), "infinity", "1", 1), strings.Replace(string(b), "keep", "lost", 1)} {
		if sameJSON(runtimeConfig(a), runtimeConfig([]byte(change))) {
			t.Fatal("lost execution metadata")
		}
		if strings.Contains(runtimeConfigDifferences(a, []byte(change)), "SECRET") {
			t.Fatal("diagnostic exposed an environment value")
		}
	}
}
