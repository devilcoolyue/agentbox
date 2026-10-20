package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// Per-request timing from the CLI itself. Claude Code measures every API
// request on the live stream — from sending it to the first stream event
// (ttft_ms) and to the end of the stream (duration_ms) — and reports both in
// the api_request event of its OpenTelemetry logs, together with the request
// id its transcript records too. Codex reports the same request in two records
// with no request id (see usage_telemetry_codex.go). The server points each
// session's CLI at a small OTLP/HTTP JSON receiver on the docker bridge — env
// for Claude, the session's config.toml for Codex — keeps only those events'
// numbers and matches them to usage rows by request id
// (store.RecordRequestTimings). Everything else in the export — prompt
// lengths, tool decisions, account attributes such as the user's email — is
// decoded and dropped, never stored or logged.
//
// The settings reach a CLI only when it starts: a terminal shell opened before
// the server began injecting the env keeps exporting nothing until the user
// opens a new tmux window or the workspace restarts, and a Codex already
// running keeps its config. Rows without telemetry keep the log-based
// estimates.

// usageTelemetryEnvNames are injected for Claude sessions. Listed for
// tmuxEnvSync so a disabled receiver stops being advertised to new windows.
var usageTelemetryEnvNames = []string{
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"OTEL_LOGS_EXPORTER",
	"OTEL_METRICS_EXPORTER",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_LOGS_EXPORT_INTERVAL",
	"CLAUDE_CODE_OTEL_SHUTDOWN_TIMEOUT_MS",
}

const (
	usageTelemetryMaxBody = 8 << 20
	// A request that takes longer than this is not one the CLI measured
	// sensibly (or the clock jumped); such a value is dropped.
	usageTelemetryMaxDuration = 6 * time.Hour
)

var apiRequestIDRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type usageTelemetryState struct {
	mu  sync.Mutex
	ln  net.Listener
	up  atomic.Bool
	err string
}

// startUsageTelemetry binds the receiver once at boot. Its address is an
// operator setting (usage_telemetry in config.json); changing it needs a
// restart, and CLIs already running keep the old URL until they restart.
func (s *Server) startUsageTelemetry() error {
	tc := s.cfg.GetUsageTelemetry()
	if tc.Disabled {
		return nil
	}
	t := &s.usageTel
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", tc.Bind)
	if err != nil {
		t.err = fmt.Sprintf("bind %s: %v", tc.Bind, err)
		return fmt.Errorf("bind usage telemetry on %s: %w", tc.Bind, err)
	}
	t.ln, t.err = ln, ""
	t.up.Store(true)
	log.Printf("usage telemetry listening on %s", ln.Addr())
	srv := &http.Server{Handler: s.admit(http.HandlerFunc(s.serveUsageTelemetry)),
		ReadHeaderTimeout: 15 * time.Second, ReadTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	release, ok := s.track(func() { _ = srv.Close(); _ = ln.Close() })
	if !ok {
		return context.Canceled
	}
	if !s.spawn(func() {
		defer release()
		defer srv.Close()
		err := srv.Serve(ln)
		t.mu.Lock()
		if t.ln == ln {
			t.ln = nil
			t.up.Store(false)
			t.err = "stopped"
			log.Printf("usage telemetry serve stopped: %v", err)
		}
		t.mu.Unlock()
	}) {
		release()
		_ = srv.Close()
		_ = ln.Close()
	}
	return nil
}

// usageTelemetryMAC authenticates a session's export path, derived from the
// auth token like the model gateway's: rotating auth_token invalidates it.
func (s *Server) usageTelemetryMAC(sessionID string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.GetAuthToken()))
	m.Write([]byte("agentbox-usage-telemetry\x00" + sessionID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:32]
}

// usageTelemetryEndpoint is the export URL a session's CLI is pointed at, or
// "" while the receiver is down: a CLI exporting into nothing would only slow
// its own exit.
func (s *Server) usageTelemetryEndpoint(sessionID string) string {
	if !s.usageTel.up.Load() {
		return ""
	}
	tc := s.cfg.GetUsageTelemetry()
	_, port, _ := net.SplitHostPort(tc.Bind)
	return "http://" + net.JoinHostPort(tc.Host, port) + "/otel/" + sessionID + "/" + s.usageTelemetryMAC(sessionID) + "/v1/logs"
}

// codexTelemetryEndpoint is the credentials service's view of the receiver:
// the export URL a Codex session's config.toml should name, or "".
func (s *Server) codexTelemetryEndpoint(sess store.Session) string {
	if sess.Agent != config.AgentCodex {
		return ""
	}
	return s.usageTelemetryEndpoint(sess.ID)
}

// usageTelemetryEnv points a Claude session's CLI at the receiver. Nothing is
// injected while the receiver is down, or when the account's env already
// configures telemetry for the operator's own collector, which we leave alone.
// A Codex session reads the URL from its config.toml (codexTelemetryEndpoint)
// and only needs the receiver kept out of a bound account proxy.
func (s *Server) usageTelemetryEnv(sess store.Session, env []string) []string {
	endpoint := s.usageTelemetryEndpoint(sess.ID)
	if endpoint == "" {
		return env
	}
	switch sess.Agent {
	case config.AgentClaude:
	case config.AgentCodex:
		return telemetryNoProxy(env, s.cfg.GetUsageTelemetry().Host)
	default:
		return env
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDE_CODE_ENABLE_TELEMETRY" || strings.HasPrefix(k, "OTEL_") {
			return env
		}
	}
	return append(telemetryNoProxy(env, s.cfg.GetUsageTelemetry().Host),
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"OTEL_LOGS_EXPORTER=otlp",
		"OTEL_METRICS_EXPORTER=none",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/json",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT="+endpoint,
		// Terminal rows show their timing a couple of seconds after the reply.
		"OTEL_LOGS_EXPORT_INTERVAL=2000",
		// Bounds how long an exiting CLI waits on the final flush.
		"CLAUDE_CODE_OTEL_SHUTDOWN_TIMEOUT_MS=1000",
	)
}

// telemetryNoProxy keeps the receiver host out of a bound account proxy: the
// export goes straight to the docker bridge.
func telemetryNoProxy(env []string, host string) []string {
	out := make([]string, 0, len(env)+len(usageTelemetryEnvNames)+2)
	proxied, bypass := false, false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			proxied = true
		case "NO_PROXY":
			// A bound account proxy must not carry the export.
			bypass = true
			if !hostListed(v, host) {
				kv = k + "=" + v + "," + host
			}
		}
		out = append(out, kv)
	}
	if proxied && !bypass {
		out = append(out, "NO_PROXY="+host, "no_proxy="+host)
	}
	return out
}

// serveUsageTelemetry accepts POST /otel/<session>/<mac>/v1/logs in OTLP
// JSON. A well-formed export always gets 200, matched or not: there is
// nothing for the CLI to retry. A storage failure gets 503 so the exporter
// may retry the batch.
func (s *Server) serveUsageTelemetry(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/otel/")
	sid, rest, _ := strings.Cut(rest, "/")
	mac, rest, _ := strings.Cut(rest, "/")
	if !ok || rest != "v1/logs" || sid == "" || !hmac.Equal([]byte(mac), []byte(s.usageTelemetryMAC(sid))) {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.store.Get(sid)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "only OTLP/HTTP JSON is accepted", http.StatusUnsupportedMediaType)
		return
	}
	body, err := readTelemetryBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTelemetryTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "unreadable export", status)
		return
	}
	records, err := decodeTelemetry(body)
	if err != nil {
		http.Error(w, "malformed OTLP JSON", http.StatusBadRequest)
		return
	}
	timings := apiRequestTimings(records)
	if sess.Agent == config.AgentCodex {
		pairs, background := s.codexPairs.feed(sess.ID, codexTelemetryEvents(records), time.Now())
		timings = append(timings, pairs...)
		if err := s.recordCodexBackground(sess, background); err != nil {
			log.Printf("usage telemetry %s: %v", sess.ID, err)
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	for i := range timings {
		timings[i].User, timings[i].SessionID = sess.User, sess.ID
	}
	if _, err := s.store.RecordRequestTimings(timings); err != nil {
		log.Printf("usage telemetry %s: %v", sess.ID, err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

var errTelemetryTooLarge = errors.New("telemetry export too large")

func readTelemetryBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(w, r.Body, usageTelemetryMaxBody)
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		src = zr
	default:
		return nil, fmt.Errorf("unsupported content encoding")
	}
	body, err := io.ReadAll(io.LimitReader(src, usageTelemetryMaxBody+1))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || len(body) > usageTelemetryMaxBody {
		return nil, errTelemetryTooLarge
	}
	return body, err
}

// otlpValue is the part of an OTLP AnyValue we read. JSON encodes int64 as a
// string by the spec, but exporters also send plain numbers; both are taken.
type otlpValue struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	BoolValue   *bool           `json:"boolValue"`
}

func (v otlpValue) int() (int64, bool) {
	if len(v.IntValue) > 0 {
		n, err := strconv.ParseInt(string(bytes.Trim(v.IntValue, `"`)), 10, 64)
		return n, err == nil
	}
	if v.DoubleValue != nil {
		return int64(*v.DoubleValue), true
	}
	if v.StringValue != nil {
		n, err := strconv.ParseInt(*v.StringValue, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (v otlpValue) str() string {
	if v.StringValue != nil {
		return *v.StringValue
	}
	return ""
}

// falsy reports an explicit false, sent as a bool or (Codex) as a string.
func (v otlpValue) falsy() bool {
	return v.BoolValue != nil && !*v.BoolValue || v.StringValue != nil && *v.StringValue == "false"
}

type otlpLogsExport struct {
	ResourceLogs []struct {
		ScopeLogs []struct {
			LogRecords []struct {
				Attributes []struct {
					Key   string    `json:"key"`
					Value otlpValue `json:"value"`
				} `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

// telemetryKeys are the only attributes kept from a log record; the rest
// (prompt lengths, account ids, the user's email) is dropped while decoding.
var telemetryKeys = map[string]bool{
	"event.name": true, "event.timestamp": true, "event.kind": true, "conversation.id": true,
	"request_id": true, "duration_ms": true, "ttft_ms": true, "endpoint": true, "success": true,
	"http.response.status_code": true, "input_token_count": true, "cached_token_count": true, "output_token_count": true,
	"model": true, "model_reasoning_effort": true, "originator": true,
}

// telemetryRecord is one log record reduced to telemetryKeys.
type telemetryRecord map[string]otlpValue

func decodeTelemetry(body []byte) ([]telemetryRecord, error) {
	var export otlpLogsExport
	if err := json.Unmarshal(body, &export); err != nil {
		return nil, err
	}
	var out []telemetryRecord
	for _, rl := range export.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				r := telemetryRecord{}
				for _, a := range rec.Attributes {
					if telemetryKeys[a.Key] {
						r[a.Key] = a.Value
					}
				}
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// parseAPIRequestTimings extracts Claude Code's api_request events from an
// export.
func parseAPIRequestTimings(body []byte) ([]store.RequestTiming, error) {
	records, err := decodeTelemetry(body)
	if err != nil {
		return nil, err
	}
	return apiRequestTimings(records), nil
}

// apiRequestTimings reads Claude Code's api_request events: the request id,
// duration_ms and, when the CLI saw a first stream event, ttft_ms. Events that
// do not add up are skipped rather than stored.
func apiRequestTimings(records []telemetryRecord) []store.RequestTiming {
	var out []store.RequestTiming
	for _, r := range records {
		id := r["request_id"].str()
		dur, hasDur := r["duration_ms"].int()
		ttft, _ := r["ttft_ms"].int()
		if r["event.name"].str() != "api_request" || !apiRequestIDRE.MatchString(id) || !hasDur ||
			dur <= 0 || dur > usageTelemetryMaxDuration.Milliseconds() || ttft < 0 || ttft > dur {
			continue
		}
		out = append(out, store.RequestTiming{RequestID: id, TTFTMs: ttft, DurationMS: dur})
	}
	return out
}
