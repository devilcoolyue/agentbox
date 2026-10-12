package dockerx

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
)

func TestCollaborationVerificationRecoveryOwnership(t *testing.T) {
	for _, scenario := range []string{"owned", "absent", "foreign", "inspect-error", "remove-error"} {
		t.Run(scenario, func(t *testing.T) {
			id := strings.Repeat("a", 32)
			removed := 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Header().Set("API-Version", "1.45")
					io.WriteString(w, "OK")
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agentbox-verify-"+id+"/json"):
					if scenario == "absent" || scenario == "inspect-error" {
						code := 404
						if scenario == "inspect-error" {
							code = 500
						}
						w.WriteHeader(code)
						io.WriteString(w, `{"message":"fixture"}`)
						return
					}
					owner := id
					if scenario == "foreign" {
						owner = "other"
					}
					json.NewEncoder(w).Encode(map[string]any{"Id": "immutable-id", "Config": map[string]any{"Labels": map[string]string{"agentbox.collaboration-reader": "1", "agentbox.collaboration-verification": owner}}})
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/immutable-id"):
					removed++
					if r.URL.Query().Get("force") != "1" {
						t.Error("helper was not force removed")
					}
					if scenario == "remove-error" {
						w.WriteHeader(500)
						io.WriteString(w, `{"message":"fixture"}`)
					} else {
						w.WriteHeader(204)
					}
				default:
					t.Errorf("unexpected Docker operation %s %s", r.Method, r.URL.Path)
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
			err = m.RemoveCollaborationVerification(t.Context(), id)
			if (err == nil) != (scenario == "owned" || scenario == "absent") {
				t.Fatal(err)
			}
			want := 0
			if scenario == "owned" || scenario == "remove-error" {
				want = 1
			}
			if removed != want {
				t.Fatal("foreign container removed or own helper leaked", removed)
			}
		})
	}
}
