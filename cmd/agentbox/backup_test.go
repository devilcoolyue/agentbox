package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupMountOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"/data", "/data/users/alice", true},
		{"/data/users/alice", "/data", true},
		{"/data", "/data", true},
		{"/data", "/database", false},
		{"/data", "/other", false},
		{"/data", "/", true},
	} {
		if got := overlaps(tc.a, tc.b); got != tc.want {
			t.Fatalf("overlaps(%q,%q)=%v", tc.a, tc.b, got)
		}
	}
}

func TestFullBackupRejectsLiveDockerMounts(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "running"}[running], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/containers/json") {
					w.Header().Set("Content-Type", "application/json")
					if !running {
						_, _ = w.Write([]byte(`[]`))
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "0123456789abcdef", "Mounts": []map[string]string{{"Source": filepath.Join(realRoot, "users"), "Destination": "/workspace", "Type": "bind"}}}})
					return
				}
				if r.URL.Path == "/_ping" {
					w.Header().Set("Api-Version", "1.45")
					_, _ = w.Write([]byte("OK"))
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
			t.Setenv("DOCKER_API_VERSION", "1.45")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			err := checkBackupContainers(t.Context(), []string{root})
			if (err != nil) != running {
				t.Fatalf("running=%v error=%v", running, err)
			}
		})
	}
}
