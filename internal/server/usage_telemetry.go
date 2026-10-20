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
// id its transcript records too. The server points each Claude session's CLI
// at a small OTLP/HTTP JSON receiver on the docker bridge, keeps only that
// event's numbers and matches them to usage rows by request id
// (store.RecordRequestTimings). Everything else in the export — prompt
// lengths, tool decisions, account attributes — is decoded and dropped, never
// stored or logged.
//
// The env reaches a CLI only when it starts: a terminal shell opened before
// the server began injecting it keeps exporting nothing until the user opens
// a new tmux window or the workspace restarts. Rows without telemetry keep the
// log-based estimates.

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

// usageTelemetryEnv points a Claude session's CLI at the receiver. Nothing is
// injected while the receiver is down — a CLI exporting into nothing would
// only slow its own exit — or when the account's env already configures
// telemetry for the operator's own collector, which we leave alone.
func (s *Server) usageTelemetryEnv(sess store.Session, env []string) []string {
	if sess.Agent != config.AgentClaude || !s.usageTel.up.Load() {
		return env
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDE_CODE_ENABLE_TELEMETRY" || strings.HasPrefix(k, "OTEL_") {
			return env
		}
	}
	tc := s.cfg.GetUsageTelemetry()
	_, port, _ := net.SplitHostPort(tc.Bind)
	endpoint := "http://" + net.JoinHostPort(tc.Host, port) + "/otel/" + sess.ID + "/" + s.usageTelemetryMAC(sess.ID) + "/v1/logs"
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
			if !hostListed(v, tc.Host) {
				kv = k + "=" + v + "," + tc.Host
			}
		}
		out = append(out, kv)
	}
	if proxied && !bypass {
		out = append(out, "NO_PROXY="+tc.Host, "no_proxy="+tc.Host)
	}
	return append(out,
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
	timings, err := parseAPIRequestTimings(body)
	if err != nil {
		http.Error(w, "malformed OTLP JSON", http.StatusBadRequest)
		return
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

// parseAPIRequestTimings extracts Claude Code's api_request events: the
// request id, duration_ms and, when the CLI saw a first stream event,
// ttft_ms. Events that do not add up are skipped rather than stored.
func parseAPIRequestTimings(body []byte) ([]store.RequestTiming, error) {
	var export otlpLogsExport
	if err := json.Unmarshal(body, &export); err != nil {
		return nil, err
	}
	var out []store.RequestTiming
	for _, rl := range export.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				var name, id string
				var ttft, dur int64
				var hasDur bool
				for _, a := range rec.Attributes {
					switch a.Key {
					case "event.name":
						if a.Value.StringValue != nil {
							name = *a.Value.StringValue
						}
					case "request_id":
						if a.Value.StringValue != nil {
							id = *a.Value.StringValue
						}
					case "duration_ms":
						dur, hasDur = a.Value.int()
					case "ttft_ms":
						ttft, _ = a.Value.int()
					}
				}
				if name != "api_request" || !apiRequestIDRE.MatchString(id) || !hasDur ||
					dur <= 0 || dur > usageTelemetryMaxDuration.Milliseconds() || ttft < 0 || ttft > dur {
					continue
				}
				out = append(out, store.RequestTiming{RequestID: id, TTFTMs: ttft, DurationMS: dur})
			}
		}
	}
	return out, nil
}
