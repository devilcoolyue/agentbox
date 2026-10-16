package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbox/internal/buildinfo"
	"agentbox/internal/config"
	"agentbox/internal/extension"
	"agentbox/internal/extension/builtin"
	"agentbox/internal/store"
	"agentbox/pkg/extspi"
	"agentbox/pkg/modelgw"
)

// Extensions (docs/architecture/extensions-spi.md): the model gateway sits
// between a workspace's CLI and its provider for accounts bound to an enabled
// extension. The CLI's base URL is rewritten per exec to a session-specific
// gateway path; the gateway forwards through the account's own egress and
// lets the extension host inspect the stream. P1 covers Claude accounts in
// API key / relay mode only.

const defaultAnthropicBaseURL = "https://api.anthropic.com"

// extensionEnvNames are rewritten by the gateway. Listed for tmuxEnvSync so a
// URL injected for an account without one is cleared again after unbinding.
var extensionEnvNames = []string{envAnthropicBaseURL}

type extensionGatewayState struct {
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

func newExtensionHost() (*extension.Host, error) {
	return extension.NewHost(extension.Options{
		HostVersion: buildinfo.Version,
		Builtins:    builtin.All(),
		OnEvent: func(e extension.Event) {
			log.Printf("extension %s: %s %s session=%s account=%s", e.Extension, e.Kind, e.Code, e.Session, e.Account)
		},
	})
}

func (s *Server) initExtensions() error {
	host, err := newExtensionHost()
	if err != nil {
		return err
	}
	s.extensions = host
	s.extGW.proxy, err = modelgw.New(modelgw.Config{Route: s.extensionRoute, StatusHeader: "X-Agentbox-Extensions",
		CodePrefix: "extension_gateway", ErrorDomain: "agentbox", MaxConcurrent: 256})
	return err
}

// applyExtensions reconciles extension instances and the gateway listener
// with the stored configuration.
func (s *Server) applyExtensions() {
	if s.extensions == nil {
		return // servers built without NewContext (tests) have no host
	}
	s.extensions.Apply(s.cfg.GetExtensions().Items)
	if err := s.applyExtensionGateway(); err != nil {
		log.Printf("extension gateway disabled: %v", err)
	}
}

func anyEnabled(items map[string]config.ExtensionSetting) bool {
	for _, item := range items {
		if item.Enabled {
			return true
		}
	}
	return false
}

// applyExtensionGateway starts the gateway once any extension is enabled. It
// keeps running after every extension is disabled: CLIs started earlier still
// hold the gateway URL and are forwarded without inspection until restarted.
func (s *Server) applyExtensionGateway() error {
	ext := s.cfg.GetExtensions()
	g := &s.extGW
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ln != nil && g.bind != ext.Gateway.Bind {
		_ = g.ln.Close()
		g.ln = nil
		g.up.Store(false)
		log.Printf("extension gateway on %s stopped", g.bind)
	}
	if g.ln != nil || !anyEnabled(ext.Items) {
		return nil
	}
	ln, err := net.Listen("tcp", ext.Gateway.Bind)
	if err != nil {
		g.err = fmt.Sprintf("bind %s: %v", ext.Gateway.Bind, err)
		return fmt.Errorf("bind extension gateway on %s: %w", ext.Gateway.Bind, err)
	}
	g.ln, g.bind, g.err = ln, ext.Gateway.Bind, ""
	g.up.Store(true)
	log.Printf("extension gateway listening on %s", ln.Addr())
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
			log.Printf("extension gateway serve stopped: %v", err)
		}
		g.mu.Unlock()
	}) {
		release()
		_ = srv.Close()
		_ = ln.Close()
	}
	return nil
}

// extensionMAC authenticates a session's gateway path. Derived from the auth
// token like proxySecret: rotating auth_token invalidates every gateway URL.
func (s *Server) extensionMAC(sessionID string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.GetAuthToken()))
	m.Write([]byte("agentbox-extension-gateway\x00" + sessionID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:32]
}

// extensionGatewayURL is the base URL a session's CLI uses instead of the
// provider's.
func (s *Server) extensionGatewayURL(sessionID string) string {
	gw := s.cfg.GetExtensions().Gateway
	_, port, _ := net.SplitHostPort(gw.Bind)
	return "http://" + net.JoinHostPort(gw.Host, port) + "/x/" + sessionID + "/" + s.extensionMAC(sessionID)
}

// claudeGatewayEligible reports whether the account's traffic can go through
// the gateway: API key / relay accounts only (decided 2026-10-16).
func claudeGatewayEligible(acct config.Account) bool {
	_, token := claudeRelay(acct)
	return acct.Type == config.AgentClaude && token != ""
}

// inspectingExtensions lists the extensions that inspect a session's model
// traffic, and whether its base URL is rewritten. A gateway that failed to
// bind leaves fail_open traffic direct and uninspected; a fail_closed binding
// still rewrites the URL so requests fail instead of bypassing inspection.
func (s *Server) inspectingExtensions(sess store.Session, acct config.Account) ([]extension.BoundExtension, bool) {
	if s.extensions == nil || sess.Agent != config.AgentClaude || !claudeGatewayEligible(acct) {
		return nil, false
	}
	bound := s.extensions.Bound(acct.ID)
	if len(bound) == 0 {
		return nil, false
	}
	if s.extGW.up.Load() {
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

// extensionEnv rewrites the exec env for a session routed through the gateway.
func (s *Server) extensionEnv(sess store.Session, acct config.Account, env []string) []string {
	if _, rewrite := s.inspectingExtensions(sess, acct); !rewrite {
		return env
	}
	gateway := s.extensionGatewayURL(sess.ID)
	host := s.cfg.GetExtensions().Gateway.Host
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case envAnthropicBaseURL:
			continue
		case "NO_PROXY", "no_proxy":
			// A bound account proxy must not carry the gateway request.
			if !hostListed(v, host) {
				kv = k + "=" + v + "," + host
			}
		}
		out = append(out, kv)
	}
	return append(out, envAnthropicBaseURL+"="+gateway)
}

func hostListed(list, host string) bool {
	for _, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == host {
			return true
		}
	}
	return false
}

// extensionRoute authenticates /x/<session>/<mac>/<api path> and resolves the
// account's real upstream and egress.
func (s *Server) extensionRoute(r *http.Request) (modelgw.Route, *modelgw.Problem) {
	notFound := &modelgw.Problem{Status: http.StatusNotFound, Code: "extension_gateway_not_found", Message: "Unknown gateway path."}
	rest, ok := strings.CutPrefix(r.URL.Path, "/x/")
	if !ok {
		return modelgw.Route{}, notFound
	}
	sid, rest, ok1 := strings.Cut(rest, "/")
	mac, rest, ok2 := strings.Cut(rest, "/")
	if !ok1 || !ok2 || sid == "" {
		return modelgw.Route{}, notFound
	}
	if !hmac.Equal([]byte(mac), []byte(s.extensionMAC(sid))) {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusForbidden, Code: "extension_gateway_forbidden", Message: "Invalid gateway credential."}
	}
	sess, ok := s.store.Get(sid)
	if !ok {
		return modelgw.Route{}, notFound
	}
	acct, err := s.sessionAccount(sess)
	if err != nil {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusForbidden, Code: "extension_gateway_forbidden", Message: "The workspace may no longer use this account."}
	}
	if sess.Agent != config.AgentClaude || acct.Type != config.AgentClaude {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadRequest, Code: "extension_gateway_unsupported", Message: "Only Claude accounts use the extension gateway."}
	}
	base := acct.Env[envAnthropicBaseURL]
	if base == "" {
		base = defaultAnthropicBaseURL
	}
	upstream, err := url.Parse(base)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadGateway, Code: "extension_gateway_upstream_error", Message: "The account's base URL is invalid."}
	}
	transport, err := s.extensionTransport(acct)
	if err != nil {
		// Fail closed: never fall back to a direct connection for an
		// account bound to an egress proxy.
		return modelgw.Route{}, &modelgw.Problem{Status: http.StatusBadGateway, Code: "extension_gateway_upstream_error", Message: "The account's egress proxy is unavailable."}
	}
	route := modelgw.Route{Upstream: upstream, Path: "/" + rest, Transport: transport}
	if claudeGatewayEligible(acct) {
		// A CLI started before unbinding still reaches the gateway; with no
		// bound extension left, Inspector is nil and the request is
		// forwarded untouched.
		if in := s.extensions.Inspector(extension.StreamContext{Session: sess.ID, Account: acct.ID, User: sess.User, Agent: sess.Agent}); in != nil {
			route.Inspector = in
		}
	}
	return route, nil
}

// extensionTransport returns the account's egress transport for the gateway,
// cached per account and rebuilt when its proxy binding changes.
func (s *Server) extensionTransport(acct config.Account) (http.RoundTripper, error) {
	p, bound := s.cfg.AccountProxy(acct.ID)
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
	g := &s.extGW
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
	t.DisableCompression = true
	t.ResponseHeaderTimeout = 5 * time.Minute
	if g.transports == nil {
		g.transports = map[string]cachedTransport{}
	}
	g.transports[acct.ID] = cachedTransport{key: key, t: t}
	return t, nil
}

// --- admin API ---

type extensionView struct {
	ID            string                   `json:"id"`
	Version       string                   `json:"version"`
	Publisher     string                   `json:"publisher"`
	Name          extension.Text           `json:"name"`
	Description   extension.Text           `json:"description"`
	License       string                   `json:"license,omitempty"`
	Homepage      string                   `json:"homepage,omitempty"`
	Builtin       bool                     `json:"builtin"`
	Runtime       string                   `json:"runtime"`
	Hooks         []string                 `json:"hooks"`
	Permissions   []string                 `json:"permissions"`
	FailurePolicy string                   `json:"failure_policy"`
	ConfigSchema  extension.ConfigSchema   `json:"config_schema"`
	Errors        []extension.ErrorDef     `json:"errors"`
	Metrics       []extension.MetricDef    `json:"metrics"`
	Setting       config.ExtensionSetting  `json:"setting"`
	Status        extension.StatusView     `json:"status"`
	Accounts      []extensionAccountChoice `json:"accounts"`
}

type extensionAccountChoice struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Eligible bool   `json:"eligible"`
}

type extensionsResponse struct {
	Gateway    extensionGatewayView `json:"gateway"`
	Extensions []extensionView      `json:"extensions"`
}

type extensionGatewayView struct {
	Bind   string `json:"bind"`
	Host   string `json:"host"`
	Active bool   `json:"active"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) extensionView(p extension.Package, items map[string]config.ExtensionSetting) extensionView {
	m := p.Manifest
	setting, ok := items[m.ID]
	if !ok {
		// Not configured yet: show the manifest defaults the form starts from.
		setting = config.ExtensionSetting{Permissions: append([]string(nil), m.Permissions...), Bindings: config.ExtensionBindings{Mode: "none"}}
		if n, err := s.extensions.Normalize(m.ID, setting); err == nil {
			setting = n
		}
	}
	if setting.Permissions == nil {
		setting.Permissions = []string{}
	}
	v := extensionView{ID: m.ID, Version: m.Version, Publisher: m.Publisher, Name: m.Name, Description: m.Description, License: m.License,
		Homepage: m.Homepage, Builtin: p.Builtin, Runtime: m.Runtime.Type, Hooks: m.Hooks, Permissions: m.Permissions,
		FailurePolicy: m.FailurePolicy, ConfigSchema: m.ConfigSchema, Errors: m.Errors, Metrics: m.Status.Metrics,
		Setting: setting, Status: s.extensions.Status(m.ID), Accounts: []extensionAccountChoice{}}
	if v.Metrics == nil {
		v.Metrics = []extension.MetricDef{}
	}
	for _, a := range s.cfg.AccountList() {
		v.Accounts = append(v.Accounts, extensionAccountChoice{ID: a.ID, Label: a.Label, Type: a.Type, Eligible: claudeGatewayEligible(a)})
	}
	return v
}

func (s *Server) extensionsResponse() extensionsResponse {
	ext := s.cfg.GetExtensions()
	s.extGW.mu.Lock()
	gw := extensionGatewayView{Bind: ext.Gateway.Bind, Host: ext.Gateway.Host, Active: s.extGW.up.Load(), Error: s.extGW.err}
	s.extGW.mu.Unlock()
	out := extensionsResponse{Gateway: gw, Extensions: []extensionView{}}
	for _, p := range s.extensions.Packages() {
		out.Extensions = append(out.Extensions, s.extensionView(p, ext.Items))
	}
	return out
}

func (s *Server) handleExtensions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.extensionsResponse())
}

func (s *Server) handleExtension(w http.ResponseWriter, r *http.Request) {
	p, ok := s.extensions.Package(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.extensionView(p, s.cfg.GetExtensions().Items))
}

type extensionUpdate struct {
	ExpectedRevision *int64                   `json:"expected_revision"`
	Enabled          bool                     `json:"enabled"`
	Order            int                      `json:"order"`
	Config           json.RawMessage          `json:"config"`
	Permissions      []string                 `json:"permissions"`
	Bindings         config.ExtensionBindings `json:"bindings"`
	FailurePolicy    string                   `json:"failure_policy"`
}

func (s *Server) handleUpdateExtension(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.extensions.Package(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	var req extensionUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ExpectedRevision == nil {
		writeErr(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	setting, err := s.extensions.Normalize(id, config.ExtensionSetting{Enabled: req.Enabled, Order: req.Order, Config: req.Config,
		Permissions: req.Permissions, Bindings: req.Bindings, FailurePolicy: req.FailurePolicy})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if setting.Enabled {
		for _, hook := range p.Manifest.Hooks {
			if need := extspi.HookPermission[hook]; !slices.Contains(setting.Permissions, need) {
				writeErr(w, http.StatusBadRequest, "启用前需要批准扩展运行所需的权限："+need)
				return
			}
		}
	}
	if _, err := s.cfg.SetExtension(id, setting, *req.ExpectedRevision); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, config.ErrExtensionRevision) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	s.applyExtensions()
	writeJSON(w, http.StatusOK, s.extensionView(p, s.cfg.GetExtensions().Items))
}

func (s *Server) handleExtensionLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.extensions.Package(id); !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.extensions.Logs(id, since)})
}

func (s *Server) handleExtensionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.extensions.Package(id); !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"events": s.extensions.Events(id)})
}
