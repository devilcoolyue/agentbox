package modelgw

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Route says where one request goes. The gateway never follows redirects and
// never retries upstream.
type Route struct {
	// Upstream is an origin or path prefix; Path is appended to it.
	Upstream *url.URL
	// Path is the request path to forward, already stripped of any routing
	// prefix the gateway itself consumed.
	Path      string
	Transport http.RoundTripper
	// StripHeaders are gateway credentials removed before forwarding.
	StripHeaders []string
	// Inspector is nil when the request is forwarded without inspection.
	Inspector Inspector
}

// Problem rejects a request before it reaches upstream.
type Problem struct {
	Status        int
	Code, Message string
}

// Verdict carries the error code and plain-text message for a rejected
// request or an interrupted stream.
type Verdict struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AbortOutcome reports whether an asynchronous interruption was accepted.
// Accepted interruptions still race the upstream: the stream's Close reason is
// the authoritative result.
type AbortOutcome string

const (
	AbortApplied    AbortOutcome = "applied"
	AbortClosed     AbortOutcome = "closed"
	AbortSuperseded AbortOutcome = "superseded"
)

// Request describes one inspectable request. Body contents are never exposed.
type Request struct {
	Protocol    Protocol
	Method      string
	Path        string // without query; Gemini may carry a key there
	Fingerprint [32]byte
	// Abort interrupts the stream asynchronously. It may be called from any
	// goroutine until Close has been delivered.
	Abort func(Verdict) AbortOutcome
}

// Inspector admits requests and observes their streams.
type Inspector interface {
	// Begin runs after the body is read and before upstream is contacted. A
	// non-nil Verdict refuses the request with HTTP 422 and the returned
	// Stream must be nil; the inspector cleans up after itself. Returning
	// (nil, nil) forwards the request without observing it.
	Begin(ctx context.Context, req Request) (Stream, *Verdict)
}

// Stream observes one admitted request. Every Stream receives exactly one
// Close. Observe runs on the forwarding goroutine and must return promptly; a
// non-nil Verdict interrupts the stream before the current event is sent.
type Stream interface {
	Open()
	Observe(deltas []Delta, ended []string) *Verdict
	Close(Close)
}

type CloseReason string

const (
	Completed     CloseReason = "completed"
	UpstreamError CloseReason = "upstream_error"
	ClientClosed  CloseReason = "client_closed"
	Aborted       CloseReason = "aborted"
	NotInspected  CloseReason = "not_inspected"
	Timeout       CloseReason = "timeout"
)

type Close struct {
	Reason  CloseReason
	Verdict *Verdict // the interruption that ended the stream, if any
}

type Config struct {
	// Route authenticates and resolves each request. It must not read the
	// body. A non-nil Problem is written as the response.
	Route func(r *http.Request) (Route, *Problem)
	// StatusHeader marks responses as "monitoring" or "not-inspected".
	StatusHeader string
	// CodePrefix names the gateway's own error codes, e.g. "loop_guard"
	// produces "loop_guard_busy".
	CodePrefix    string
	ErrorDomain   string
	MaxConcurrent int           // default 64
	Timeout       time.Duration // per request, default 30 minutes
}

// Proxy is safe for concurrent requests. It supports HTTP SSE, not WebSockets
// or CONNECT tunnels.
type Proxy struct {
	cfg    Config
	active chan struct{}
}

func New(cfg Config) (*Proxy, error) {
	if cfg.Route == nil {
		return nil, errors.New("modelgw: Route is required")
	}
	if cfg.StatusHeader == "" {
		cfg.StatusHeader = "X-Model-Gateway-Status"
	}
	if cfg.CodePrefix == "" {
		cfg.CodePrefix = "model_gateway"
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 64
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	return &Proxy{cfg: cfg, active: make(chan struct{}, cfg.MaxConcurrent)}, nil
}

func (p *Proxy) code(suffix string) string { return p.cfg.CodePrefix + "_" + suffix }

// abortState serializes interruption with stream completion.
type abortState struct {
	mu      sync.Mutex
	closed  bool
	verdict *Verdict
	cancel  context.CancelFunc
}

func (s *abortState) abort(v Verdict) AbortOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return AbortClosed
	case s.verdict != nil:
		return AbortSuperseded
	}
	s.verdict = &v
	s.cancel()
	return AbortApplied
}

func (s *abortState) current() *Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verdict
}

func (s *abortState) finish() *Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.verdict
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, problem := p.cfg.Route(r)
	if problem != nil {
		WriteError(w, problem.Status, problem.Code, problem.Message)
		return
	}
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" {
		WriteError(w, http.StatusBadRequest, p.code("unsupported_transport"), "Use HTTP SSE streaming.")
		return
	}
	if route.Upstream == nil || route.Transport == nil {
		WriteError(w, http.StatusBadGateway, p.code("upstream_error"), "Upstream request failed.")
		return
	}
	select {
	case p.active <- struct{}{}:
		defer func() { <-p.active }()
	default:
		WriteError(w, http.StatusServiceUnavailable, p.code("busy"), "Gateway concurrency limit reached.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.Timeout)
	defer cancel()
	out := r.Clone(ctx)
	out.URL.Scheme, out.URL.Host = route.Upstream.Scheme, route.Upstream.Host
	out.URL.Path = strings.TrimRight(route.Upstream.Path, "/") + "/" + strings.TrimLeft(route.Path, "/")
	out.URL.RawPath = ""
	out.Host, out.RequestURI = route.Upstream.Host, ""
	out.Header = r.Header.Clone()
	StripHopHeaders(out.Header)
	for _, h := range route.StripHeaders {
		out.Header.Del(h)
	}
	out.Header.Set("Accept-Encoding", "identity")

	protocol := ProtocolFor(route.Path)
	state := &abortState{cancel: cancel}
	var stream Stream
	if route.Inspector != nil && r.Method == http.MethodPost && protocol != "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
		if err != nil {
			WriteError(w, http.StatusRequestEntityTooLarge, p.code("request_limit"), "Request could not be read within the 32 MiB limit.")
			return
		}
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.TransferEncoding = nil
		var reject *Verdict
		stream, reject = route.Inspector.Begin(ctx, Request{Protocol: protocol, Method: r.Method, Path: route.Path, Fingerprint: Fingerprint(r, body), Abort: state.abort})
		if reject != nil {
			// This is a terminal rejection, not a rate limit. Retry-After makes
			// Codex wait out a cooldown and launch the same model call again.
			WriteError(w, http.StatusUnprocessableEntity, reject.Code, reject.Message)
			return
		}
	}
	closed := false
	closeWith := func(reason CloseReason) {
		if stream == nil || closed {
			return
		}
		closed = true
		v := state.finish()
		if v != nil && reason != Completed {
			reason = Aborted
		}
		stream.Close(Close{Reason: reason, Verdict: v})
	}
	defer closeWith(UpstreamError) // panics and unexpected returns still close

	resp, err := route.Transport.RoundTrip(out)
	if err != nil {
		if v := state.current(); v != nil {
			WriteError(w, http.StatusUnprocessableEntity, v.Code, v.Message)
		} else {
			// Transport errors can contain credential-bearing URLs. Do not echo.
			WriteError(w, http.StatusBadGateway, p.code("upstream_error"), "Upstream request failed.")
		}
		closeWith(UpstreamError)
		return
	}
	defer resp.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	sse := mediaType == "text/event-stream"
	guarded := stream != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && sse
	if guarded && resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		WriteError(w, http.StatusBadGateway, p.code("encoding"), "Upstream must honor Accept-Encoding: identity for SSE inspection.")
		closeWith(UpstreamError)
		return
	}
	StripHopHeaders(resp.Header)
	for k, values := range resp.Header {
		w.Header()[k] = values
	}
	w.Header().Set(p.cfg.StatusHeader, "not-inspected")
	controller := http.NewResponseController(w)
	if !guarded {
		closeWith(NotInspected)
		w.WriteHeader(resp.StatusCode)
		if !sse {
			io.Copy(w, resp.Body)
			return
		}
		// Uninspected SSE must still reach the client event by event.
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				_ = controller.Flush()
			}
			if err != nil {
				return
			}
		}
	}
	w.Header().Del("Content-Length")
	w.Header().Del("ETag")
	w.Header().Set(p.cfg.StatusHeader, "monitoring")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	_ = controller.Flush()
	stream.Open()
	decoder := NewDecoder(protocol).WithDomain(p.cfg.ErrorDomain)
	fail := func(v Verdict) {
		if r.Context().Err() == nil {
			_, _ = w.Write(decoder.Failure(v.Code, v.Message))
			_ = controller.Flush()
		}
	}
	reader := bufio.NewReaderSize(resp.Body, 32<<10)
	for {
		event, err := ReadEvent(reader)
		if err != nil && err != io.EOF {
			cancel()
			_ = resp.Body.Close()
			if v := state.current(); v != nil {
				fail(*v)
				closeWith(Aborted)
				return
			}
			reason := UpstreamError
			switch {
			case r.Context().Err() != nil:
				reason = ClientClosed
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				reason = Timeout
			}
			fail(Verdict{Code: p.code("stream_error"), Message: "Upstream stream was interrupted or exceeded the 1 MiB event limit."})
			closeWith(reason)
			return
		}
		if len(event) != 0 {
			if deltas, ended := decoder.Observe(event); len(deltas) != 0 || len(ended) != 0 {
				if hit := stream.Observe(deltas, ended); hit != nil {
					state.abort(*hit)
				}
			}
			if v := state.current(); v != nil {
				// Cancel our upstream request. The relay must propagate this
				// cancellation for its own model generation to stop as well.
				cancel()
				_ = resp.Body.Close()
				fail(*v)
				closeWith(Aborted)
				return
			}
			if _, writeErr := w.Write(event); writeErr != nil {
				closeWith(ClientClosed)
				return
			}
			if flushErr := controller.Flush(); flushErr != nil {
				closeWith(ClientClosed)
				return
			}
		}
		if err == io.EOF {
			closeWith(Completed)
			return
		}
	}
}
