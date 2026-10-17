package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
	"agentbox/internal/extension"
	"agentbox/internal/store"
	"agentbox/pkg/extspi"
	"agentbox/pkg/modelgw"
)

// The model gateway (docs/architecture/extensions-spi.md §8) is the
// infrastructure behind the model.* extension hooks: it sits between a
// workspace's CLI and its provider for accounts bound to an enabled extension
// that uses those hooks. The CLI's base URL points at a session-specific
// gateway path; the gateway forwards through the account's own egress and
// lets the extension host inspect the stream. Only API key / relay accounts
// are routed (decided 2026-10-16), for both agents:
//
//   - Claude reads ANTHROPIC_BASE_URL, rewritten per exec (modelGatewayEnv);
//   - Codex reads base_url from the session's .codex/config.toml, rewritten
//     by the credentials service (codexEndpoint) on start and broadcast, and
//     reconciled here when bindings change. The account pool keeps the real
//     upstream, which the gateway reads back per request.
//
// Extensions without model hooks never start it.

const defaultAnthropicBaseURL = "https://api.anthropic.com"

// modelHooks are the extension hooks served by the gateway.
var modelHooks = []string{extspi.HookModelRequest, extspi.HookModelStream}

// modelGatewayEnvNames are rewritten by the gateway. Listed for tmuxEnvSync so
// a URL injected for an account without one is cleared again after unbinding.
var modelGatewayEnvNames = []string{envAnthropicBaseURL}

type modelGatewayState struct {
	mu   sync.Mutex
	ln   net.Listener
	bind string
	up   atomic.Bool
	err  string

	proxy *modelgw.Proxy

	tmu        sync.Mutex
	transports map[string]cachedTransport // by account ID
}

type cachedTransport struct {
	key string
	t   *http.Transport
}

func (s *Server) initModelGateway() (err error) {
	s.modelGW.proxy, err = modelgw.New(modelgw.Config{Route: s.modelGatewayRoute, StatusHeader: "X-Agentbox-Extensions",
		CodePrefix: "model_gateway", ErrorDomain: "agentbox", MaxConcurrent: 256})
	return err
}

// applyModelGateway starts the gateway once an enabled extension uses a model
// hook. It keeps running after they are all disabled: CLIs started earlier
// still hold the gateway URL and are forwarded without inspection until
// restarted.
func (s *Server) applyModelGateway() error {
	gwc := s.cfg.GetModelGateway()
	g := &s.modelGW
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ln != nil && g.bind != gwc.Bind {
		_ = g.ln.Close()
		g.ln = nil
		g.up.Store(false)
		log.Printf("model gateway on %s stopped", g.bind)
	}
	if g.ln != nil || !s.extensions.Uses(modelHooks...) {
		return nil
	}
	ln, err := net.Listen("tcp", gwc.Bind)
	if err != nil {
		g.err = fmt.Sprintf("bind %s: %v", gwc.Bind, err)
		return fmt.Errorf("bind model gateway on %s: %w", gwc.Bind, err)
	}
	g.ln, g.bind, g.err = ln, gwc.Bind, ""
	g.up.Store(true)
	log.Printf("model gateway listening on %s", ln.Addr())
	srv := &http.Server{Handler: s.admit(g.proxy), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	release, ok := s.track(func() { _ = srv.Close(); _ = ln.Close() })
	if !ok {
		return context.Canceled
	}
	if !s.spawn(func() {
		defer release()
		defer srv.Close()
		err := srv.Serve(ln)
		g.mu.Lock()
		if g.ln == ln {
			g.ln = nil
			g.up.Store(false)
			g.err = "stopped"
			log.Printf("model gateway serve stopped: %v", err)
		}
		g.mu.Unlock()
	}) {
		release()
		_ = srv.Close()
		_ = ln.Close()
	}
	return nil
}

// modelGatewayView is the gateway's state for the settings page.
type modelGatewayView struct {
	Bind                 string `json:"bind"`
	Host                 string `json:"host"`
	DedicatedConnections bool   `json:"dedicated_connections"`
	Active               bool   `json:"active"`
	// Needed reports whether an enabled extension uses a model hook; the
	// gateway only listens then.
	Needed bool   `json:"needed"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) modelGatewayView() modelGatewayView {
	gwc := s.cfg.GetModelGateway()
	v := modelGatewayView{Bind: gwc.Bind, Host: gwc.Host, DedicatedConnections: gwc.DedicatedConnections}
	if s.extensions != nil {
		v.Needed = s.extensions.Uses(modelHooks...)
	}
	s.modelGW.mu.Lock()
	v.Active, v.Error = s.modelGW.up.Load(), s.modelGW.err
	s.modelGW.mu.Unlock()
	return v
}

// modelGatewayMAC authenticates a session's gateway path. Derived from the
// auth token like proxySecret: rotating auth_token invalidates every gateway
// URL. The domain string predates the gateway's own name and stays, so URLs
// held by running CLIs survive upgrades.
func (s *Server) modelGatewayMAC(sessionID string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.GetAuthToken()))
	m.Write([]byte("agentbox-extension-gateway\x00" + sessionID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:32]
}

// modelGatewayURL is the base URL a session's CLI uses instead of the
// provider's.
func (s *Server) modelGatewayURL(sessionID string) string {
	gwc := s.cfg.GetModelGateway()
	_, port, _ := net.SplitHostPort(gwc.Bind)
	return "http://" + net.JoinHostPort(gwc.Host, port) + "/x/" + sessionID + "/" + s.modelGatewayMAC(sessionID)
}

// claudeGatewayEligible reports whether a Claude account's traffic can go
// through the gateway: API key / relay accounts only (decided 2026-10-16).
func claudeGatewayEligible(acct config.Account) bool {
	_, token := claudeRelay(acct)
	return acct.Type == config.AgentClaude && token != ""
}

// gatewayEligible covers both agents: Claude API key / relay accounts, and
// Codex accounts logged in with an API key (official or relay endpoint).
// Subscription and ChatGPT logins stay direct.
func gatewayEligible(acct config.Account) bool {
	switch acct.Type {
	case config.AgentClaude:
		return claudeGatewayEligible(acct)
	case config.AgentCodex:
		_, ok := credentials.CodexUpstream(acct)
		return ok
	}
	return false
}

// inspectingExtensions lists the extensions that inspect a session's model
// traffic, and whether its base URL is rewritten. A gateway that failed to
// bind leaves fail_open traffic direct and uninspected; a fail_closed binding
// still rewrites the URL so requests fail instead of bypassing inspection.
func (s *Server) inspectingExtensions(sess store.Session, acct config.Account) ([]extension.BoundExtension, bool) {
	if s.extensions == nil || sess.Agent != acct.Type {
		return nil, false
	}
	// Bindings first: they are in memory, while Codex eligibility reads the
	// account pool and the workspace list asks for every session.
	bound := s.extensions.Bound(acct.ID)
	if len(bound) == 0 || !gatewayEligible(acct) {
		return nil, false
	}
	if s.modelGW.up.Load() {
		return bound, true
	}
	var closed []extension.BoundExtension
	for _, b := range bound {
		if b.Policy == extension.FailClosed {
			closed = append(closed, b)
		}
	}
	return closed, len(closed) != 0
}

// modelGatewayEnv rewrites the exec env for a session routed through the
// gateway. Both agents keep the gateway out of a bound account proxy; only
// Claude takes its endpoint from the env (Codex's is in config.toml, see
// codexEndpoint).
func (s *Server) modelGatewayEnv(sess store.Session, acct config.Account, env []string) []string {
	if _, rewrite := s.inspectingExtensions(sess, acct); !rewrite {
		return env
	}
	claude := sess.Agent == config.AgentClaude
	host := s.cfg.GetModelGateway().Host
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case envAnthropicBaseURL:
			if claude {
				continue
			}
		case "NO_PROXY", "no_proxy":
			// A bound account proxy must not carry the gateway request.
			if !hostListed(v, host) {
				kv = k + "=" + v + "," + host
			}
		}
		out = append(out, kv)
	}
	if claude {
		out = append(out, envAnthropicBaseURL+"="+s.modelGatewayURL(sess.ID))
	}
	return out
}

// codexEndpoint is the credentials service's view of the gateway: the base
// URL a Codex session's config.toml should name, or "" for the account's own
// upstream.
func (s *Server) codexEndpoint(acct config.Account, sess store.Session) string {
	if sess.Agent != config.AgentCodex {
		return ""
	}
	if _, rewrite := s.inspectingExtensions(sess, acct); !rewrite {
		return ""
	}
	return s.modelGatewayURL(sess.ID)
}

// applyCodexEndpoint is the workspace start hook: every chat turn and
// terminal starts a fresh CLI that rereads config.toml.
func (s *Server) applyCodexEndpoint(ctx context.Context, sess store.Session, acct config.Account) error {
	if sess.Agent != config.AgentCodex {
		return nil
	}
	return s.credentialService().ApplyCodexEndpoint(ctx, acct, sess)
}

// reconcileCodexEndpoints rewrites seeded Codex session configs after the
// bindings, the gateway or its address changed, so the next CLI start in a
// running container follows the new state without waiting for a restart.
func (s *Server) reconcileCodexEndpoints(ctx context.Context) {
	for _, sess := range s.store.All() {
		if ctx.Err() != nil {
			return
		}
		if sess.Agent != config.AgentCodex {
			continue
		}
		acct, err := s.sessionAccount(sess)
		if err != nil || acct.CredentialsDir == "" {
			continue
		}
		if err := s.credentialService().ApplyCodexEndpoint(ctx, acct, sess); err != nil {
			log.Printf("model gateway: session %s Codex endpoint: %v", sess.ID, err)
		}
	}
}

func hostListed(list, host string) bool {
	for _, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == host {
			return true
		}
	}
	return false
}

// modelGatewayRoute authenticates /x/<session>/<mac>/<api path> and resolves
// the account's real upstream and egress.
func (s *Server) modelGatewayRoute(r *http.Request) (modelgw.Route, *modelgw.Problem) {
	notFound := &modelgw.Problem{Status: http.StatusNotFound, Code: "model_gateway_not_found", Message: "Unknown gateway path."}
	rest, ok := strings.CutPrefix(r.URL.Path, "/x/")
	if !ok {
		return modelgw.Route{}, notFound
	}
	sid, rest, ok1 := strings.Cut(rest, "/")
	mac, rest, ok2 := strings.Cut(rest, "/")
	if !ok1 || !ok2 || sid == "" {
		return modelgw.Route{}, notFound
	}
	if !hmac.Equal([]byte(mac), []byte(s.modelGatewayMAC(sid))) {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusForbidden, Code: "model_gateway_forbidden", Message: "Invalid gateway credential."}
	}
	sess, ok := s.store.Get(sid)
	if !ok {
		return modelgw.Route{}, notFound
	}
	acct, err := s.sessionAccount(sess)
	if err != nil {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusForbidden, Code: "model_gateway_forbidden", Message: "The workspace may no longer use this account."}
	}
	if sess.Agent != acct.Type {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadRequest, Code: "model_gateway_unsupported", Message: "The workspace's agent does not match its account."}
	}
	var base string
	switch acct.Type {
	case config.AgentClaude:
		if base = acct.Env[envAnthropicBaseURL]; base == "" {
			base = defaultAnthropicBaseURL
		}
	case config.AgentCodex:
		// The pool keeps the real endpoint; a CLI started before the account
		// switched to a ChatGPT login has no API key upstream left to reach.
		var ok bool
		if base, ok = credentials.CodexUpstream(acct); !ok {
			return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadRequest, Code: "model_gateway_unsupported", Message: "The account no longer uses an API key; restart the conversation or terminal."}
		}
	default:
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadRequest, Code: "model_gateway_unsupported", Message: "This account type does not use the model gateway."}
	}
	upstream, err := url.Parse(base)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadGateway, Code: "model_gateway_upstream_error", Message: "The account's base URL is invalid."}
	}
	transport, err := s.modelGatewayTransport(acct)
	if err != nil {
		// Fail closed: never fall back to a direct connection for an
		// account bound to an egress proxy.
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadGateway, Code: "model_gateway_upstream_error", Message: "The account's egress proxy is unavailable."}
	}
	route := modelgw.Route{Upstream: upstream, Path: "/" + rest, Transport: transport}
	if gatewayEligible(acct) {
		// A CLI started before unbinding still reaches the gateway; with no
		// bound extension left, Inspector is nil and the request is
		// forwarded untouched.
		if in := s.extensions.Inspector(extension.StreamContext{Session: sess.ID, Account: acct.ID, User: sess.User, Agent: sess.Agent}); in != nil {
			route.Inspector = in
		}
	}
	return route, nil
}

// modelGatewayTransport returns the account's egress transport for the
// gateway, cached per account and rebuilt when its proxy binding or the
// gateway's connection mode changes.
func (s *Server) modelGatewayTransport(acct config.Account) (http.RoundTripper, error) {
	p, bound := s.cfg.AccountProxy(acct.ID)
	dedicated := s.cfg.GetModelGateway().DedicatedConnections
	key := "direct"
	if bound {
		if p.Disabled {
			return nil, errors.New("proxy disabled")
		}
		if !config.ValidProxyScheme(p.Scheme) {
			return nil, errors.New("invalid proxy scheme")
		}
		sum := sha256.Sum256([]byte(p.ID + "\x00" + p.Scheme + "\x00" + p.Host + "\x00" + strconv.Itoa(p.Port) + "\x00" + p.Username + "\x00" + p.Password))
		key = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	if dedicated {
		key += "+dedicated"
	}
	g := &s.modelGW
	g.tmu.Lock()
	defer g.tmu.Unlock()
	if c, ok := g.transports[acct.ID]; ok {
		if c.key == key {
			return c.t, nil
		}
		c.t.CloseIdleConnections()
	}
	var t *http.Transport
	if bound {
		t = proxyTransport(p)
	} else {
		t = http.DefaultTransport.(*http.Transport).Clone()
	}
	if dedicated {
		// One HTTP/1.1 TCP/TLS connection per request: interrupting a stream
		// closes our hop instead of resetting one HTTP/2 stream on a shared
		// connection. It cannot make a relay stop its own upstream request.
		t = modelgw.DedicatedTransport(t)
	}
	t.DisableCompression = true
	t.ResponseHeaderTimeout = 5 * time.Minute
	if g.transports == nil {
		g.transports = map[string]cachedTransport{}
	}
	g.transports[acct.ID] = cachedTransport{key: key, t: t}
	return t, nil
}
