package loopguard

import (
	"agentbox/pkg/modelgw"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

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
	proxy       *modelgw.Proxy
	upstream    *url.URL
	token       string
	tokenHeader string
	transport   http.RoundTripper
	onTrip      func(Detection)
	cooldown    *Cooldown
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
		transport = DedicatedTransport(t)
	}
	g := &Gateway{upstream: u, token: cfg.Token, tokenHeader: header, transport: transport, onTrip: cfg.OnTrip,
		cooldown: NewCooldown(retryCooldown)}
	g.proxy, err = modelgw.New(modelgw.Config{Route: g.route, StatusHeader: "X-Loop-Guard-Status", CodePrefix: "loop_guard", ErrorDomain: "loopguard"})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// DedicatedTransport returns a copy of t that opens a separate HTTP/1.1
// TCP/TLS connection per request, so cancellation closes the connection
// itself. The caller's transport is not modified.
func DedicatedTransport(t *http.Transport) *http.Transport {
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
	return t
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.proxy.ServeHTTP(w, r) }

func (g *Gateway) route(r *http.Request) (modelgw.Route, *modelgw.Problem) {
	if g.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(g.tokenHeader)), []byte(g.token)) != 1 {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusUnauthorized, Code: "loop_guard_unauthorized", Message: "Invalid gateway token."}
	}
	return modelgw.Route{Upstream: g.upstream, Path: r.URL.Path, Transport: g.transport,
		StripHeaders: []string{"X-Loop-Guard-Token"}, Inspector: g}, nil
}

// Begin bounds blind harness retries without retaining credentials or prompts.
func (g *Gateway) Begin(_ context.Context, req modelgw.Request) (modelgw.Stream, *modelgw.Verdict) {
	if g.cooldown.Blocked(req.Fingerprint) {
		return nil, &modelgw.Verdict{Code: ErrorCode, Message: CooldownMessage(retryCooldown)}
	}
	return &gatewayStream{g: g, key: req.Fingerprint, monitor: NewMonitor(0, 0)}, nil
}

// CooldownMessage explains a cooldown rejection.
func CooldownMessage(ttl time.Duration) string {
	return fmt.Sprintf("An identical request recently looped. Change the context before retrying, or wait %d seconds.", int(ttl/time.Second))
}

// TripMessage explains an interruption; it contains no model output.
func TripMessage(d Detection) string {
	return fmt.Sprintf("Model %s was interrupted: its last %d short sentences contain only %d distinct phrases. Continue with corrected context; this reply did not complete.", d.Channel, d.Window, d.Distinct)
}

type gatewayStream struct {
	g       *Gateway
	key     [32]byte
	monitor *Monitor
}

func (s *gatewayStream) Open() {}

func (s *gatewayStream) Observe(deltas []modelgw.Delta, ended []string) *modelgw.Verdict {
	hit := s.monitor.Push(deltas, ended)
	if hit == nil {
		return nil
	}
	s.g.cooldown.Block(s.key)
	if s.g.onTrip != nil {
		s.g.onTrip(*hit)
	}
	return &modelgw.Verdict{Code: ErrorCode, Message: TripMessage(*hit)}
}

func (s *gatewayStream) Close(modelgw.Close) {}

// maxCooldowns bounds the cooldown table.
const maxCooldowns = 1024

// Cooldown remembers request fingerprints that recently looped. It keeps only
// hashes and expiry times, at most 1024 entries, and is safe for concurrent
// use. A zero TTL disables it.
type Cooldown struct {
	ttl     time.Duration
	mu      sync.Mutex
	blocked map[[32]byte]time.Time
}

func NewCooldown(ttl time.Duration) *Cooldown {
	return &Cooldown{ttl: ttl, blocked: make(map[[32]byte]time.Time)}
}

// Blocked reports whether key is still cooling down.
func (c *Cooldown) Blocked(key [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	return time.Now().Before(c.blocked[key])
}

// Block starts a cooldown for key, evicting the entry closest to expiry when
// the table is full.
func (c *Cooldown) Block(key [32]byte) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.expireLocked(now)
	if _, ok := c.blocked[key]; !ok && len(c.blocked) >= maxCooldowns {
		var oldest [32]byte
		var earliest time.Time
		for k, until := range c.blocked {
			if earliest.IsZero() || until.Before(earliest) {
				oldest, earliest = k, until
			}
		}
		delete(c.blocked, oldest)
	}
	c.blocked[key] = now.Add(c.ttl)
}

// Len returns the number of active cooldowns.
func (c *Cooldown) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	return len(c.blocked)
}

func (c *Cooldown) expireLocked(now time.Time) {
	for k, until := range c.blocked {
		if !now.Before(until) {
			delete(c.blocked, k)
		}
	}
}
