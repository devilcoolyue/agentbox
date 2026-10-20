package server

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// The fixture copies the shape of a real Claude Code 2.1.303 export (event
// names, value types, other events in the same batch); every value is
// synthetic.
func readOTLPFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/otlp-logs-claude-code-2.1.303.json")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParseAPIRequestTimings(t *testing.T) {
	got, err := parseAPIRequestTimings(readOTLPFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []store.RequestTiming{
		{RequestID: "req_011SyntheticToolUse0001", TTFTMs: 4758, DurationMS: 5252},
		// int64 as a JSON string, as the OTLP spec writes it.
		{RequestID: "req_011SyntheticText000002", TTFTMs: 1502, DurationMS: 12696},
		// No first stream event: the duration alone still counts.
		{RequestID: "req_011SyntheticNoFirst003", TTFTMs: 0, DurationMS: 800},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("timings = %+v", got)
	}
	// Events that do not add up are skipped, not stored.
	bad := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[
		{"attributes":[{"key":"event.name","value":{"stringValue":"api_request"}},{"key":"request_id","value":{"stringValue":"req_a"}},{"key":"duration_ms","value":{"intValue":100}},{"key":"ttft_ms","value":{"intValue":200}}]},
		{"attributes":[{"key":"event.name","value":{"stringValue":"api_request"}},{"key":"request_id","value":{"stringValue":"req b"}},{"key":"duration_ms","value":{"intValue":100}}]},
		{"attributes":[{"key":"event.name","value":{"stringValue":"api_request"}},{"key":"request_id","value":{"stringValue":"req_c"}}]},
		{"attributes":[{"key":"event.name","value":{"stringValue":"api_request"}},{"key":"request_id","value":{"stringValue":"req_d"}},{"key":"duration_ms","value":{"intValue":"99999999999"}}]},
		{"attributes":[{"key":"event.name","value":{"stringValue":"api_error"}},{"key":"request_id","value":{"stringValue":"req_e"}},{"key":"duration_ms","value":{"intValue":100}}]}
	]}]}]}`
	if got, err := parseAPIRequestTimings([]byte(bad)); err != nil || len(got) != 0 {
		t.Fatalf("bad events = %+v, %v", got, err)
	}
	if _, err := parseAPIRequestTimings([]byte("not json")); err == nil {
		t.Fatal("malformed export accepted")
	}
}

func telemetryServer(t *testing.T) (*Server, store.Session) {
	t.Helper()
	s, sess := newTestServer(t)
	s.cfg.AuthToken = "test-token"
	sess.Agent = config.AgentClaude
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	return s, sess
}

func postTelemetry(s *Server, path string, body []byte, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.serveUsageTelemetry(w, req)
	return w
}

func TestUsageTelemetryFillsRows(t *testing.T) {
	s, sess := telemetryServer(t)
	if err := s.store.InsertUsage(store.UsageEvent{User: sess.User, SessionID: sess.ID, TurnID: "t", Agent: "claude",
		OutputTokens: 1097, APIRequestID: "req_011SyntheticText000002", TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	path := "/otel/" + sess.ID + "/" + s.usageTelemetryMAC(sess.ID) + "/v1/logs"
	if w := postTelemetry(s, path, readOTLPFixture(t), nil); w.Code != http.StatusOK || w.Body.String() != "{}" {
		t.Fatalf("export = %d %s", w.Code, w.Body)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: sess.User})
	if len(rows) != 1 || rows[0].APITTFTMs != 1502 || rows[0].APIDurationMS != 12696 {
		t.Fatalf("rows = %+v", rows)
	}
	// The request without a row yet waits for it, whichever comes first.
	if err := s.store.UpsertTerminalUsage(store.UsageEvent{User: sess.User, SessionID: sess.ID, TurnID: "t2", Agent: "claude",
		ReqID: "req_011SyntheticToolUse0001", APIRequestID: "req_011SyntheticToolUse0001", WallMS: 5100, TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.store.ListUsage(store.UsageFilter{User: sess.User}) {
		if r.APIRequestID == "req_011SyntheticToolUse0001" && (r.APITTFTMs != 4758 || r.APIDurationMS != 5252) {
			t.Fatalf("waiting timing not claimed: %+v", r)
		}
	}

	// gzip is accepted like the OTLP exporters may send it.
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(readOTLPFixture(t))
	_ = zw.Close()
	if w := postTelemetry(s, path, zipped.Bytes(), map[string]string{"Content-Encoding": "gzip"}); w.Code != http.StatusOK {
		t.Fatalf("gzip export = %d %s", w.Code, w.Body)
	}
}

func TestUsageTelemetryRejects(t *testing.T) {
	s, sess := telemetryServer(t)
	good := "/otel/" + sess.ID + "/" + s.usageTelemetryMAC(sess.ID) + "/v1/logs"
	other := store.Session{ID: "s2", User: "bob", Name: "x"}
	if err := s.store.Put(other); err != nil {
		t.Fatal(err)
	}
	body := readOTLPFixture(t)
	for name, c := range map[string]struct {
		path   string
		header map[string]string
		body   []byte
		want   int
	}{
		"another session's MAC": {path: "/otel/" + sess.ID + "/" + s.usageTelemetryMAC(other.ID) + "/v1/logs", body: body, want: 404},
		"no MAC":                {path: "/otel/" + sess.ID + "//v1/logs", body: body, want: 404},
		"unknown session":       {path: "/otel/gone/" + s.usageTelemetryMAC("gone") + "/v1/logs", body: body, want: 404},
		"metrics path":          {path: strings.Replace(good, "/v1/logs", "/v1/metrics", 1), body: body, want: 404},
		"protobuf":              {path: good, header: map[string]string{"Content-Type": "application/x-protobuf"}, body: body, want: 415},
		"malformed":             {path: good, body: []byte("{"), want: 400},
		"too large":             {path: good, body: bytes.Repeat([]byte(" "), usageTelemetryMaxBody+1), want: 413},
	} {
		if w := postTelemetry(s, c.path, c.body, c.header); w.Code != c.want {
			t.Errorf("%s: status %d, want %d", name, w.Code, c.want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, good, nil)
	w := httptest.NewRecorder()
	s.serveUsageTelemetry(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", w.Code)
	}
}

func telemetryEnvValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

func TestUsageTelemetryEnv(t *testing.T) {
	s, sess := telemetryServer(t)
	base := []string{"ANTHROPIC_MODEL=claude-opus-5-5"}
	if env := s.usageTelemetryEnv(sess, base); len(env) != 1 {
		t.Fatalf("receiver down but env injected: %v", env)
	}
	s.usageTel.up.Store(true)
	env := s.usageTelemetryEnv(sess, base)
	if v, _ := telemetryEnvValue(env, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); v != "http://172.17.0.1:1084/otel/s1/"+s.usageTelemetryMAC("s1")+"/v1/logs" {
		t.Fatalf("endpoint = %q", v)
	}
	for _, k := range usageTelemetryEnvNames {
		if _, ok := telemetryEnvValue(env, k); !ok {
			t.Errorf("%s missing", k)
		}
	}
	if v, _ := telemetryEnvValue(env, "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"); v != "http/json" {
		t.Fatalf("protocol = %q", v)
	}
	// A bound account proxy must not carry the export.
	proxied := s.usageTelemetryEnv(sess, []string{"HTTPS_PROXY=http://u:p@172.17.0.1:1081", "NO_PROXY=localhost", "no_proxy=localhost,172.17.0.1"})
	if v, _ := telemetryEnvValue(proxied, "NO_PROXY"); v != "localhost,172.17.0.1" {
		t.Fatalf("NO_PROXY = %q", v)
	}
	if v, _ := telemetryEnvValue(proxied, "no_proxy"); v != "localhost,172.17.0.1" {
		t.Fatalf("no_proxy = %q", v)
	}
	bare := s.usageTelemetryEnv(sess, []string{"HTTPS_PROXY=http://relay.example:8080"})
	if v, _ := telemetryEnvValue(bare, "NO_PROXY"); v != "172.17.0.1" {
		t.Fatalf("NO_PROXY without one = %q", v)
	}
	// The operator's own collector configured on the account wins.
	own := []string{"CLAUDE_CODE_ENABLE_TELEMETRY=1", "OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example"}
	if env := s.usageTelemetryEnv(sess, own); !slices.Equal(env, own) {
		t.Fatalf("account telemetry overridden: %v", env)
	}
	codex := sess
	codex.Agent = config.AgentCodex
	if env := s.usageTelemetryEnv(codex, base); len(env) != 1 {
		t.Fatalf("Codex got Claude telemetry env: %v", env)
	}
	// New tmux windows stop seeing the receiver once it is gone.
	if sync := tmuxEnvSync(nil); !strings.Contains(sync, "set-environment -gu OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") || !strings.Contains(sync, "set-environment -gu CLAUDE_CODE_ENABLE_TELEMETRY") {
		t.Fatalf("tmux sync does not clear telemetry env: %s", sync)
	}
}

func TestUsageTelemetryConfig(t *testing.T) {
	c := &config.Config{}
	if got := c.GetUsageTelemetry(); got.Bind != "172.17.0.1:1084" || got.Host != "172.17.0.1" || got.Disabled {
		t.Fatalf("defaults = %+v", got)
	}
}
