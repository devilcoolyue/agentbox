package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agentbox/internal/diagnostics"
	"agentbox/internal/store"
)

func TestEnvironmentCheckCancellationDoesNotReportSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"synthetic-secret","data_dir":".","timezone":"UTC","accounts":[{"id":"fixture","type":"claude","env":{"ANTHROPIC_API_KEY":"synthetic-key"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); w.WriteHeader(503) }))
	defer server.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.45")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	var out bytes.Buffer
	if err := checkConfig(ctx, []string{"--config", path, "--environment"}, &out); err == nil {
		t.Fatal("interrupted environment check returned success")
	}
	var result struct {
		Valid       bool
		Environment diagnostics.Report
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Valid || result.Environment.HasFailures() {
		t.Fatal("cancellation invented a configuration/runtime failure", out.String())
	}
	for _, c := range result.Environment.Checks {
		if c.ID == "docker" && (c.State != diagnostics.NotChecked || c.Code != "check_cancelled") {
			t.Fatal("interrupted ping misclassified")
		}
	}
}

func TestEnvironmentCheckDockerFailureAndNoRawDetails(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			config := `{"auth_token":"private-secret","data_dir":".","timezone":"UTC","agent_image":"private-image","accounts":[{"id":"private-account","type":"claude","env":{"ANTHROPIC_API_KEY":"private-key"}}]}`
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" && !(r.Method == "HEAD" && r.URL.Path == "/_ping") {
					t.Error("diagnosis attempted Docker mutation", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/_ping" {
					_, _ = w.Write([]byte("OK"))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/images/private-image/json") {
					if missing {
						w.WriteHeader(404)
						_, _ = w.Write([]byte(`{"message":"private-key /private/docker-path"}`))
					} else {
						_, _ = w.Write([]byte(`{"Id":"synthetic-image"}`))
					}
					return
				}
				t.Error("unexpected Docker endpoint", r.URL.Path)
				w.WriteHeader(404)
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
			t.Setenv("DOCKER_API_VERSION", "1.45")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			var out bytes.Buffer
			err := checkConfig(t.Context(), []string{"--config", path, "--environment"}, &out)
			ownershipDenied := runtime.GOOS == "linux" && os.Geteuid() != 0 && os.Geteuid() != 1000
			if (err != nil) != (missing || ownershipDenied) {
				t.Fatal("wrong exit status", err, out.String())
			}
			var result struct {
				Valid       bool
				Environment diagnostics.Report
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Valid {
				t.Fatal("Docker failure changed config validity")
			}
			for _, c := range result.Environment.Checks {
				if c.ID == "agent_image" && (c.State == diagnostics.Failed) != missing {
					t.Fatal("wrong image result")
				}
				if c.ID == "model" && c.State != diagnostics.NotChecked {
					t.Fatal("model availability invented")
				}
			}
			for _, secret := range []string{"private-secret", "private-image", "private-account", "private-key", dir} {
				if strings.Contains(out.String(), secret) {
					t.Fatal("report leaked private data")
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
				t.Fatal("probe left files or initialized instance")
			}
		})
	}
}

func TestConfigCheckRemainsOfflineAndDoesNotCreateState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"private-secret","data_dir":"missing-data","timezone":"UTC","accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "invalid Docker configuration must not be used")
	var output bytes.Buffer
	if err := checkConfig(t.Context(), []string{"--config", path}, &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Valid       bool
		Schema      int `json:"schema_version"`
		Epoch       int `json:"compatibility_epoch"`
		Environment diagnostics.Report
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Valid || result.Schema != store.SchemaVersion || result.Epoch != 1 {
		t.Fatal("deployment compatibility fields changed")
	}
	for _, c := range result.Environment.Checks {
		if c.ID != "configuration" && c.State != diagnostics.NotChecked {
			t.Fatal("offline check performed environment work")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "missing-data")); !os.IsNotExist(err) {
		t.Fatal("created missing data directory")
	}
}

func TestEnvironmentCheckInvalidConfigIsRedacted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"private-secret","timezone":"private-user-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := checkConfig(t.Context(), []string{"--config", path, "--environment"}, &output)
	if err == nil {
		t.Fatal("invalid config passed")
	}
	for _, secret := range []string{"private-secret", "private-user-secret", dir} {
		if strings.Contains(output.String()+err.Error(), secret) {
			t.Fatal("config error leaked")
		}
	}
	var result struct {
		Valid       bool
		Environment diagnostics.Report
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Valid || !result.Environment.HasFailures() {
		t.Fatal("failure missing from report")
	}
}
