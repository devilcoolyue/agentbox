package loopguard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxEventBytes = 1 << 20
const maxRequestBytes = 32 << 20
const retryCooldown = 30 * time.Second

// Config configures a fixed upstream reverse proxy. Upstream is an origin or
// path prefix, not an API endpoint: incoming paths are appended to it.
type Config struct {
	Upstream string
	Token    string // optional X-Loop-Guard-Token, stripped before forwarding
	// TokenHeader may be Authorization to reuse an existing provider Bearer
	// credential. In that mode Token includes "Bearer " and is forwarded.
	TokenHeader string
	// DedicatedConnections uses HTTP/1.1 without keep-alive so cancellation
	// closes this request's TCP/TLS connection instead of an HTTP/2 stream.
	DedicatedConnections bool
	Transport            http.RoundTripper
	// OnTrip receives metadata only; it must be concurrency-safe and return
	// promptly. It must not schedule model requests.
	OnTrip func(Detection)
}

// Gateway supports HTTP SSE, not WebSockets or encrypted CONNECT tunnels.
// A shared instance is safe for concurrent requests. It never retries upstream.
type Gateway struct {
	upstream    *url.URL
	token       string
	tokenHeader string
	transport   http.RoundTripper
	onTrip      func(Detection)
	mu          sync.Mutex
	blocked     map[[32]byte]time.Time
	active      chan struct{}
}

func New(cfg Config) (*Gateway, error) {
	header := http.CanonicalHeaderKey(cfg.TokenHeader)
	if header == "" {
		header = "X-Loop-Guard-Token"
	}
	if header != "X-Loop-Guard-Token" && header != "Authorization" {
		return nil, errors.New("token header must be X-Loop-Guard-Token or Authorization")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("upstream must be an http(s) origin or path prefix without credentials, query or fragment")
	}
	transport := cfg.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 30 * time.Second
		t.DisableCompression = true
		transport = t
	}
	if cfg.DedicatedConnections {
		t, ok := transport.(*http.Transport)
		if !ok {
			return nil, errors.New("dedicated connections require an http.Transport")
		}
		t = t.Clone()
		t.Protocols = new(http.Protocols)
		t.Protocols.SetHTTP1(true)
		t.ForceAttemptHTTP2 = false
		t.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		if t.TLSClientConfig == nil {
			t.TLSClientConfig = &tls.Config{}
		} else {
			t.TLSClientConfig = t.TLSClientConfig.Clone()
		}
		t.TLSClientConfig.NextProtos = []string{"http/1.1"}
		t.DisableKeepAlives = true
		transport = t
	}
	return &Gateway{upstream: u, token: cfg.Token, tokenHeader: header, transport: transport, onTrip: cfg.OnTrip,
		blocked: make(map[[32]byte]time.Time), active: make(chan struct{}, 64)}, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(g.tokenHeader)), []byte(g.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "loop_guard_unauthorized", "Invalid gateway token.")
		return
	}
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" {
		writeError(w, http.StatusBadRequest, "loop_guard_unsupported_transport", "Use HTTP SSE streaming.")
		return
	}
	select {
	case g.active <- struct{}{}:
		defer func() { <-g.active }()
	default:
		writeError(w, http.StatusServiceUnavailable, "loop_guard_busy", "Gateway concurrency limit reached.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	out := r.Clone(ctx)
	out.URL.Scheme, out.URL.Host = g.upstream.Scheme, g.upstream.Host
	out.URL.Path = strings.TrimRight(g.upstream.Path, "/") + "/" + strings.TrimLeft(r.URL.Path, "/")
	out.URL.RawPath = ""
	out.Host, out.RequestURI = g.upstream.Host, ""
	out.Header = r.Header.Clone()
	stripHopHeaders(out.Header)
	out.Header.Del("X-Loop-Guard-Token")
	out.Header.Set("Accept-Encoding", "identity")
	p := protocolFor(r.URL.Path)
	var fingerprint [32]byte
	if r.Method == http.MethodPost && p != "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "loop_guard_request_limit", "Request could not be read within the 32 MiB limit.")
			return
		}
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.TransferEncoding = nil
		// Bound blind harness retries without retaining credentials or prompts.
		h := sha256.New()
		for _, value := range []string{r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Goog-Api-Key")} {
			fmt.Fprintf(h, "%d:", len(value))
			io.WriteString(h, value)
		}
		h.Write(fingerprintBody(body))
		copy(fingerprint[:], h.Sum(nil))
		if g.cooling(fingerprint, false) {
			// This is a terminal rejection, not a rate limit. Retry-After makes
			// Codex wait out the cooldown and launch the same model call again.
			writeError(w, http.StatusUnprocessableEntity, ErrorCode, "An identical request recently looped. Change the context before retrying, or wait 30 seconds.")
			return
		}
	}
	resp, err := g.transport.RoundTrip(out)
	if err != nil {
		// Transport errors can contain credential-bearing URLs. Do not echo.
		writeError(w, http.StatusBadGateway, "loop_guard_upstream_error", "Upstream request failed.")
		return
	}
	defer resp.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	guarded := r.Method == http.MethodPost && p != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 && mediaType == "text/event-stream"
	if guarded && resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		writeError(w, http.StatusBadGateway, "loop_guard_encoding", "Upstream must honor Accept-Encoding: identity for SSE inspection.")
		return
	}
	stripHopHeaders(resp.Header)
	for k, values := range resp.Header {
		w.Header()[k] = values
	}
	w.Header().Set("X-Loop-Guard-Status", "not-inspected")
	if !guarded {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	w.Header().Del("Content-Length")
	w.Header().Del("ETag")
	w.Header().Set("X-Loop-Guard-Status", "monitoring")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	guard := &streamGuard{protocol: p}
	reader := bufio.NewReaderSize(resp.Body, 32<<10)
	for {
		event, err := readEvent(reader)
		if err != nil && err != io.EOF {
			cancel()
			_ = resp.Body.Close()
			if r.Context().Err() == nil {
				_, _ = w.Write(guard.failure("loop_guard_stream_error", "Upstream stream was interrupted or exceeded the 1 MiB event limit."))
				_ = controller.Flush()
			}
			return
		}
		if len(event) != 0 {
			if hit := guard.observe(event); hit != nil {
				g.cooling(fingerprint, true)
				// Cancel our upstream request. The relay must propagate this
				// cancellation for its own model generation to stop as well.
				cancel()
				_ = resp.Body.Close()
				message := fmt.Sprintf("Model %s was interrupted: its last %d short sentences contain only %d distinct phrases. Continue with corrected context; this reply did not complete.", hit.Channel, hit.Window, hit.Distinct)
				_, _ = w.Write(guard.failure(ErrorCode, message))
				_ = controller.Flush()
				if g.onTrip != nil {
					g.onTrip(*hit)
				}
				return
			}
			if _, writeErr := w.Write(event); writeErr != nil {
				return
			}
			if flushErr := controller.Flush(); flushErr != nil {
				return
			}
		}
		if err == io.EOF {
			return
		}
	}
}

// Codex reserializes nested objects in a different key order on reconnect.
// Hash JSON structurally, but send the original bytes upstream unchanged.
// UseNumber preserves large integers without float64 rounding collisions.
func fingerprintBody(body []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return body
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return body
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return normalized
}

// readEvent preserves upstream bytes, including CRLF, comments and multi-line
// data fields. Its bound applies even to a malicious line without a newline.
func readEvent(r *bufio.Reader) ([]byte, error) {
	var event []byte
	lineStart := 0
	for {
		// ReadByte still uses the buffered reader. ReadSlice could wait for a
		// whole additional buffer after the limit, leaving a too-large event
		// running if the provider pauses before its next newline.
		b, err := r.ReadByte()
		if err != nil {
			return event, err
		}
		if len(event) == maxEventBytes {
			return nil, errors.New("event limit")
		}
		event = append(event, b)
		if b != '\n' {
			continue
		}
		line := event[lineStart:]
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return event, nil
		}
		lineStart = len(event)
	}
}

func (g *Gateway) cooling(key [32]byte, insert bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for k, until := range g.blocked {
		if !now.Before(until) {
			delete(g.blocked, k)
		}
	}
	if !insert {
		return now.Before(g.blocked[key])
	}
	if len(g.blocked) >= 1024 {
		var oldest [32]byte
		var earliest time.Time
		for k, until := range g.blocked {
			if earliest.IsZero() || until.Before(earliest) {
				oldest, earliest = k, until
			}
		}
		delete(g.blocked, oldest)
	}
	g.blocked[key] = now.Add(retryCooldown)
	return false
}

func stripHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, k := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": code, "code": code, "message": message}})
}
