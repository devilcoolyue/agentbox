package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/pelletier/go-toml/v2"
)

const telemetryURL = "http://172.17.0.1:1084/otel/s1/mac/v1/logs"

func otelEndpoint(settings map[string]any) string {
	otel, _ := settings["otel"].(map[string]any)
	exporter, _ := otel["exporter"].(map[string]any)
	h, _ := exporter["otlp-http"].(map[string]any)
	endpoint, _ := h["endpoint"].(string)
	return endpoint
}

func TestCodexTelemetryConfig(t *testing.T) {
	base := []byte("model = \"m\"\nmodel_provider = \"openai\"\n")
	next, err := codexTelemetryConfig(base, telemetryURL, "s1")
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{}
	if err := toml.Unmarshal(next, &settings); err != nil {
		t.Fatal(err)
	}
	otel := settings["otel"].(map[string]any)
	h := otel["exporter"].(map[string]any)["otlp-http"].(map[string]any)
	if h["endpoint"] != telemetryURL || h["protocol"] != "json" || otel["log_user_prompt"] != false || settings["model"] != "m" {
		t.Fatalf("config = %s", next)
	}
	// Codex rewrites the file with expanded tables; it is still ours, and an
	// unchanged section marshals to the same bytes.
	expanded := []byte("model = 'm'\nmodel_provider = 'openai'\n\n[otel]\nlog_user_prompt = false\n\n[otel.exporter]\n[otel.exporter.otlp-http]\n" +
		"endpoint = 'http://10.0.0.1:9999/otel/s1/old/v1/logs'\nprotocol = 'json'\n\n[tui]\nscreen_reader_detection_done = true\n")
	moved, err := codexTelemetryConfig(expanded, telemetryURL, "s1")
	if err != nil {
		t.Fatal(err)
	}
	settings = map[string]any{}
	_ = toml.Unmarshal(moved, &settings)
	if otelEndpoint(settings) != telemetryURL || settings["tui"] == nil {
		t.Fatalf("receiver move not followed: %s", moved)
	}
	if again, _ := codexTelemetryConfig(moved, telemetryURL, "s1"); string(again) != string(moved) {
		t.Fatalf("not stable:\n%s\n%s", moved, again)
	}
	// Receiver off: our section goes, everything else stays.
	off, err := codexTelemetryConfig(moved, "", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "otel") || !strings.Contains(string(off), "screen_reader_detection_done") {
		t.Fatalf("off = %s", off)
	}
	if same, _ := codexTelemetryConfig(base, "", "s1"); string(same) != string(base) {
		t.Fatalf("config without telemetry rewritten: %s", same)
	}
	// The operator's own collector, or another session's path, is left alone.
	for _, own := range []string{
		"[otel]\nexporter = { otlp-http = { endpoint = \"https://otel.example/v1/logs\", protocol = \"binary\" } }\n",
		"[otel]\nexporter = { otlp-http = { endpoint = \"http://172.17.0.1:1084/otel/s2/mac/v1/logs\", protocol = \"json\" } }\n",
		"[otel]\nenvironment = \"prod\"\n",
	} {
		for _, endpoint := range []string{telemetryURL, ""} {
			if got, _ := codexTelemetryConfig([]byte(own), endpoint, "s1"); string(got) != own {
				t.Fatalf("operator otel changed (%q): %s", endpoint, got)
			}
		}
	}
}

func TestApplyCodexEndpointWritesTelemetrySessionOnly(t *testing.T) {
	acct := config.Account{ID: "sub", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	put(t, filepath.Join(acct.CredentialsDir, "auth.json"), `{"tokens":{"access_token":"a","refresh_token":"r","id_token":"i"}}`)
	pool := "cli_auth_credentials_store = 'file'\nforced_login_method = 'chatgpt'\nmodel_provider = 'openai'\n"
	put(t, filepath.Join(acct.CredentialsDir, "config.toml"), pool)
	svc, data := codexGatewayService(t, acct, func(config.Account, store.Session) string { return "" })
	on := true
	svc.SetCodexTelemetry(func(sess store.Session) string {
		if on {
			return "http://172.17.0.1:1084/otel/" + sess.ID + "/mac/v1/logs"
		}
		return ""
	})
	put(t, filepath.Join(codexHome(data), "config.toml"), "model = 'session-model'\n"+pool)
	sess := store.Session{ID: "s1", User: "alice", Agent: config.AgentCodex, AccountID: acct.ID}
	if err := svc.ApplyCodexEndpoint(t.Context(), acct, sess); err != nil {
		t.Fatal(err)
	}
	home := readTOML(t, filepath.Join(codexHome(data), "config.toml"))
	if otelEndpoint(home) != "http://172.17.0.1:1084/otel/s1/mac/v1/logs" || home["model"] != "session-model" {
		t.Fatalf("telemetry not written: %v", home)
	}
	if got, _ := os.ReadFile(filepath.Join(acct.CredentialsDir, "config.toml")); string(got) != pool {
		t.Fatalf("telemetry URL leaked into the account pool: %s", got)
	}
	on = false
	if err := svc.ApplyCodexEndpoint(t.Context(), acct, sess); err != nil {
		t.Fatal(err)
	}
	if home := readTOML(t, filepath.Join(codexHome(data), "config.toml")); home["otel"] != nil || home["model"] != "session-model" {
		t.Fatalf("telemetry not removed: %v", home)
	}
}
