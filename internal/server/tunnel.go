// Reverse tunnel: a user runs the abox-link client on their machine and dials
// GET /api/tunnel. The server keeps that yamux session and exposes a single
// SOCKS5 proxy (bound on the docker bridge gateway) that every container can
// reach. A container's SOCKS request is authenticated by the owning user's
// per-user secret, then routed down that user's tunnel and dialed on their
// machine — so the agent reaches the user's LAN/intranet without the server
// having any route to it.
package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	socks5 "github.com/things-go/go-socks5"

	"agentbox/internal/store"
	"agentbox/internal/tunnel"
)

// tunnelHub tracks live per-user tunnels and their stable SOCKS secrets. The
// secret persists for a user across link reconnects so proxy credentials
// injected into a long-running container exec stay valid.
type tunnelHub struct {
	mu      sync.RWMutex
	conns   map[string]*yamux.Session // user -> live session (absent when down)
	secrets map[string]string         // user -> stable SOCKS password
	maps    map[string]*userMaps      // user -> live port-map listeners
	info    map[string]connInfo       // user -> live connection facts (for UI)

	// proxyUp is set once the shared SOCKS listener is actually bound; env
	// injection checks it so containers never get a proxy URL to a dead port.
	proxyUp atomic.Bool
}

// connInfo is what the status API shows about a live tunnel connection.
type connInfo struct {
	Since  time.Time
	Remote string
}

func newTunnelHub() *tunnelHub {
	return &tunnelHub{
		conns:   map[string]*yamux.Session{},
		secrets: map[string]string{},
		maps:    map[string]*userMaps{},
		info:    map[string]connInfo{},
	}
}

// mapEntry is one live port-map listener: connections to ln are piped down the
// user's tunnel to the fixed Target.
type mapEntry struct {
	Port   int
	Target string
	ln     net.Listener
}

// userMaps groups the entries bound by one tunnel connection. The pointer
// identity lets a stale connection's cleanup tear down only its own generation
// — a reconnect that re-bound maps must not be clobbered.
type userMaps struct{ entries []mapEntry }

// setMaps makes entries the user's current port maps, closing any previous
// generation's listeners (the newest link's --map set is authoritative, even
// when empty). The returned release closes this generation and is safe to call
// after a newer one superseded it.
func (h *tunnelHub) setMaps(user string, entries []mapEntry) (release func()) {
	cur := &userMaps{entries: entries}
	h.mu.Lock()
	old := h.maps[user]
	h.maps[user] = cur
	h.mu.Unlock()
	if old != nil {
		for _, e := range old.entries {
			_ = e.ln.Close()
		}
	}
	return func() {
		h.mu.Lock()
		if h.maps[user] == cur {
			delete(h.maps, user)
		}
		h.mu.Unlock()
		for _, e := range cur.entries {
			_ = e.ln.Close()
		}
	}
}

// mapsFor snapshots the user's live port maps (feeds env injection).
func (h *tunnelHub) mapsFor(user string) []mapEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if um := h.maps[user]; um != nil {
		return um.entries
	}
	return nil
}

// serveMapListener accepts connections for one map until its listener closes.
func (h *tunnelHub) serveMapListener(user, target string, ln net.Listener, authorize func(user, srcIP string) bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go h.serveMapConn(user, target, conn, authorize)
	}
}

// serveMapConn relays one mapped connection down the user's tunnel. The raw
// TCP listener carries no credentials, so source identity is the only
// isolation between users: authorize gates each connection by source IP
// (the owning user's containers, or the host itself).
func (h *tunnelHub) serveMapConn(user, target string, conn net.Conn, authorize func(user, srcIP string) bool) {
	src, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		_ = conn.Close()
		return
	}
	if authorize != nil && !authorize(user, src) {
		log.Printf("tunnel map: DENY %s -> %s (source is not a container of %q)", src, target, user)
		_ = conn.Close()
		return
	}
	sess := h.session(user)
	if sess == nil {
		_ = conn.Close()
		return
	}
	st, err := sess.OpenStream()
	if err != nil {
		_ = conn.Close()
		return
	}
	closeBoth := func() { _ = st.Close(); _ = conn.Close() }
	if err := tunnel.WriteConnect(st, target); err != nil {
		closeBoth()
		return
	}
	_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
	var status [1]byte
	if _, err := io.ReadFull(st, status[:]); err != nil || status[0] != tunnel.StatusOK {
		log.Printf("tunnel map: %s -> %s failed (status=%d err=%v)", src, target, status[0], err)
		closeBoth()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	tunnel.Pipe(conn, st)
}

// register makes sess the current tunnel for user, minting the user's secret on
// first use, and returns an unregister func that only clears the entry if it is
// still this exact session (so a later reconnect isn't clobbered).
func (h *tunnelHub) register(user string, sess *yamux.Session, remote string) (secret string, unregister func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.conns[user]; old != nil && old != sess {
		_ = old.Close() // one tunnel per user; supersede the stale one
	}
	if h.secrets[user] == "" {
		h.secrets[user] = newToken()
	}
	h.conns[user] = sess
	h.info[user] = connInfo{Since: time.Now(), Remote: remote}
	secret = h.secrets[user]
	return secret, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.conns[user] == sess {
			delete(h.conns, user)
			delete(h.info, user)
		}
	}
}

// connInfoFor returns the live connection facts for user (ok=false when down).
func (h *tunnelHub) connInfoFor(user string) (connInfo, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ci, ok := h.info[user]
	return ci, ok && h.conns[user] != nil
}

// onlineUsers lists users with a live tunnel (admin status view).
func (h *tunnelHub) onlineUsers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.conns))
	for u := range h.conns {
		out = append(out, u)
	}
	return out
}

// closeAll kicks every live tunnel (feature turned off at runtime). Each WS
// handler then unregisters itself and releases its port maps.
func (h *tunnelHub) closeAll() {
	h.mu.RLock()
	sessions := make([]*yamux.Session, 0, len(h.conns))
	for _, s := range h.conns {
		sessions = append(sessions, s)
	}
	h.mu.RUnlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

func (h *tunnelHub) session(user string) *yamux.Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[user]
}

// proxyCreds returns the user's SOCKS password and whether a tunnel is live.
func (h *tunnelHub) proxyCreds(user string) (secret string, up bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.conns[user] == nil {
		return "", false
	}
	return h.secrets[user], true
}

// Valid implements socks5.CredentialStore: the SOCKS username is the agentbox
// user and the password is that user's tunnel secret.
func (h *tunnelHub) Valid(user, password, _ string) bool {
	h.mu.RLock()
	secret := h.secrets[user]
	live := h.conns[user] != nil
	h.mu.RUnlock()
	if !live || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(secret)) == 1
}

// passThroughResolver leaves FQDNs unresolved so the target hostname reaches
// our Dial verbatim and is resolved on the link (socks5h semantics) — essential
// for names that only resolve inside the user's network.
type passThroughResolver struct{}

func (passThroughResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	return ctx, nil, nil
}

// socksServer builds the shared SOCKS5 proxy: it authenticates each request by
// the owning user's tunnel secret, routes CONNECT down that user's tunnel, and
// keeps FQDNs unresolved so the link does DNS (socks5h).
func (h *tunnelHub) socksServer() *socks5.Server {
	dial := func(ctx context.Context, network, addr string, req *socks5.Request) (net.Conn, error) {
		user := ""
		if req.AuthContext != nil {
			user = req.AuthContext.Payload["username"]
		}
		sess := h.session(user)
		if sess == nil {
			return nil, fmt.Errorf("no live tunnel for user %q", user)
		}
		st, err := sess.OpenStream()
		if err != nil {
			return nil, fmt.Errorf("open tunnel stream: %w", err)
		}
		if err := tunnel.WriteConnect(st, addr); err != nil {
			_ = st.Close()
			return nil, err
		}
		// Bound the handshake so a wedged link can't hold this container
		// connection open forever; the link dials with a 10s timeout, so 15s
		// covers a legitimately slow target. Cleared once OK arrives.
		deadline := time.Now().Add(15 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = st.SetReadDeadline(deadline)
		var status [1]byte
		if _, err := io.ReadFull(st, status[:]); err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("tunnel stream: no status (timeout or closed): %w", err)
		}
		switch status[0] {
		case tunnel.StatusOK:
			_ = st.SetReadDeadline(time.Time{}) // relay has no deadline
			// go-socks5 derives the CONNECT reply's bound address from the
			// conn's LocalAddr and rejects anything that isn't *net.TCPAddr; a
			// raw yamux stream isn't, so present a valid zero address.
			return &boundConn{Conn: st}, nil
		case tunnel.StatusDenied:
			_ = st.Close()
			return nil, fmt.Errorf("target %s denied by %s's local whitelist", addr, user)
		default:
			_ = st.Close()
			return nil, fmt.Errorf("link failed to dial %s", addr)
		}
	}
	return socks5.NewServer(
		socks5.WithAuthMethods([]socks5.Authenticator{socks5.UserPassAuthenticator{Credentials: h}}),
		socks5.WithResolver(passThroughResolver{}),
		socks5.WithDialAndRequest(dial),
	)
}

// applyTunnel reconciles the running SOCKS listener with the current config:
// it starts the proxy when the feature is enabled, stops it (and kicks live
// links) when disabled, and rebinds on a proxy_bind change. Called at boot and
// after every settings save, so the toggle is hot — no restart needed. A bind
// failure is returned but is non-fatal (the caller logs / shows it and the
// server keeps running without the tunnel).
func (s *Server) applyTunnel() error {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	tc := s.cfg.GetTunnel()
	if s.tunnelLn != nil && (!tc.Enabled || s.tunnelBind != tc.ProxyBind) {
		_ = s.tunnelLn.Close()
		s.tunnelLn = nil
		s.tunnels.proxyUp.Store(false)
		log.Printf("tunnel SOCKS5 proxy on %s stopped", s.tunnelBind)
	}
	if !tc.Enabled {
		s.tunnels.closeAll() // live links are kicked; reconnects now get 403
		return nil
	}
	if s.tunnelLn != nil {
		return nil
	}
	ln, err := net.Listen("tcp", tc.ProxyBind)
	if err != nil {
		return fmt.Errorf("bind tunnel proxy on %s: %w", tc.ProxyBind, err)
	}
	s.tunnelLn = ln
	s.tunnelBind = tc.ProxyBind
	s.tunnels.proxyUp.Store(true)
	log.Printf("tunnel SOCKS5 proxy listening on %s", tc.ProxyBind)
	srv := s.tunnels.socksServer()
	go func() {
		err := srv.Serve(ln)
		// Only report down if we are still the current listener — a rebind
		// closes us on purpose and has already flipped the state itself.
		s.tunnelMu.Lock()
		if s.tunnelLn == ln {
			s.tunnelLn = nil
			s.tunnels.proxyUp.Store(false)
			log.Printf("tunnel proxy serve stopped: %v", err)
		}
		s.tunnelMu.Unlock()
	}()
	go s.warnTunnelReachability(tc.ProxyBind, s.cfg.GetContainer().Network)
	return nil
}

// warnTunnelReachability logs a loud warning when the proxy bind address does
// not match the gateway of the network containers actually run on — a common
// silent-failure mode (e.g. proxy on the default bridge but sessions on a
// custom network, which cannot route to 172.17.0.1).
func (s *Server) warnTunnelReachability(bind, sessionNetwork string) {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gw, err := s.dock.NetworkGateway(ctx, sessionNetwork)
	if err != nil {
		log.Printf("tunnel: could not verify reachability (network %q gateway unknown): %v", sessionNetwork, err)
		return
	}
	if host != gw && !net.ParseIP(host).IsUnspecified() {
		log.Printf("tunnel: WARNING proxy_bind host %s != %q network gateway %s; containers may be unable to reach the proxy — bind to %s:<port> or set container.network accordingly",
			host, sessionNetwork, gw, gw)
	}
}

// maxMapsPerUser caps how many gateway ports one link may claim.
const maxMapsPerUser = 16

// bindUserMaps binds the link-declared port maps on the proxy host. Each spec
// gets an individual status ("ok" or the failure reason) reported back to the
// link in the handshake response; one failed bind stops neither the others nor
// the tunnel itself. Always called (even with no specs) so the newest link's
// declaration supersedes any previous generation's listeners.
func (s *Server) bindUserMaps(user string, specs []tunnel.MapSpec) (statuses []string, release func()) {
	bindHost := ""
	if h, _, err := net.SplitHostPort(s.cfg.GetTunnel().ProxyBind); err == nil {
		bindHost = h
	}
	var entries []mapEntry
	for _, spec := range specs {
		status := "ok"
		switch {
		case spec.Port < 1024:
			status = "listen ports below 1024 are not allowed"
		case len(entries) >= maxMapsPerUser:
			status = fmt.Sprintf("too many maps (max %d)", maxMapsPerUser)
		default:
			ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(spec.Port)))
			if err != nil {
				status = err.Error()
			} else {
				entries = append(entries, mapEntry{Port: spec.Port, Target: spec.Target, ln: ln})
				go s.tunnels.serveMapListener(user, spec.Target, ln, s.authorizeMapSource)
				log.Printf("tunnel map for %q: %s -> %s", user, ln.Addr(), spec.Target)
			}
		}
		statuses = append(statuses, fmt.Sprintf("%d=%s", spec.Port, headerSafe(status)))
	}
	return statuses, s.tunnels.setMaps(user, entries)
}

// headerSafe flattens a status message for the comma-separated response header.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == ',':
			return ';'
		case r < 0x20:
			return ' '
		}
		return r
	}, s)
}

// mapSourceAuth caches the per-user container-IP set so each mapped-port
// connection does not cost docker inspect round-trips.
type mapSourceAuth struct {
	mu sync.Mutex
	m  map[string]mapSourceEntry
}

type mapSourceEntry struct {
	ips map[string]bool
	at  time.Time
}

// authorizeMapSource reports whether a connection from srcIP may use user's
// mapped ports: yes for the host itself (the proxy bind address, i.e. the
// docker gateway) and for that user's own containers; other users' containers
// are refused.
func (s *Server) authorizeMapSource(user, srcIP string) bool {
	if h, _, err := net.SplitHostPort(s.cfg.GetTunnel().ProxyBind); err == nil && h == srcIP {
		return true
	}
	if s.userContainerIPs(user, false)[srcIP] {
		return true
	}
	// Refresh once: the connection may come from a container started after the
	// cached set was built.
	return s.userContainerIPs(user, true)[srcIP]
}

// userContainerIPs returns the IP set of the user's containers, cached briefly.
func (s *Server) userContainerIPs(user string, refresh bool) map[string]bool {
	s.mapAuth.mu.Lock()
	ent, ok := s.mapAuth.m[user]
	s.mapAuth.mu.Unlock()
	if ok && !refresh && time.Since(ent.at) < 10*time.Second {
		return ent.ips
	}
	ips := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, sess := range s.store.All() {
		if sess.User != user || sess.ContainerID == "" {
			continue
		}
		for _, ip := range s.dock.ContainerIPs(ctx, sess.ContainerID) {
			ips[ip] = true
		}
	}
	s.mapAuth.mu.Lock()
	if s.mapAuth.m == nil {
		s.mapAuth.m = map[string]mapSourceEntry{}
	}
	s.mapAuth.m[user] = mapSourceEntry{ips: ips, at: time.Now()}
	s.mapAuth.mu.Unlock()
	return ips
}

// handleTunnelWS accepts the link's WebSocket, wraps it in a yamux session (the
// server is the yamux client) and keeps it registered until it closes.
func (s *Server) handleTunnelWS(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.GetTunnel().Enabled {
		writeErr(w, http.StatusForbidden, "tunnel feature disabled")
		return
	}
	user := reqUser(r).Name
	specs, err := tunnel.ParseMapSpecs(r.Header.Get(tunnel.MapsHeader))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Bind declared port maps before upgrading so per-map results ride the
	// handshake response for the link to log.
	statuses, releaseMaps := s.bindUserMaps(user, specs)
	respHdr := http.Header{}
	if len(statuses) > 0 {
		respHdr.Set(tunnel.MapStatusHeader, strings.Join(statuses, ","))
	}
	conn, err := s.upgrader.Upgrade(w, r, respHdr)
	if err != nil {
		releaseMaps()
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	sess, err := yamux.Client(tunnel.NewWSConn(conn), cfg)
	if err != nil {
		releaseMaps()
		_ = conn.Close()
		return
	}
	_, unregister := s.tunnels.register(user, sess, r.RemoteAddr)
	log.Printf("tunnel up for user %q from %s (%d port maps)", user, r.RemoteAddr, len(specs))
	<-sess.CloseChan()
	unregister()
	releaseMaps()
	_ = sess.Close()
	_ = conn.Close()
	log.Printf("tunnel down for user %q", user)
}

// tunnelEnvList returns the proxy env injected into a session's execs when the
// owning user has a live tunnel. It is intentionally NOT a global HTTP(S)_PROXY:
// forcing all traffic (including model-API calls) through the user's home
// network would be slow and brittle. The agent instead uses
// AGENTBOX_INTRANET_PROXY explicitly for intranet targets, e.g.
// `curl --proxy "$AGENTBOX_INTRANET_PROXY" http://gitlab.corp.local/...`.
func (s *Server) tunnelEnvList(sess store.Session) []string {
	tc := s.cfg.GetTunnel()
	if !tc.Enabled {
		return nil
	}
	secret, up := s.tunnels.proxyCreds(sess.User)
	if !up {
		return nil
	}
	var env []string
	if s.tunnels.proxyUp.Load() {
		proxyURL := fmt.Sprintf("socks5h://%s:%s@%s:%s", sess.User, secret, tc.ProxyHost, portOf(tc.ProxyBind))
		env = append(env, "AGENTBOX_INTRANET_PROXY="+proxyURL)
	}
	// Port maps are independent listeners: they work even if the shared SOCKS
	// proxy failed to bind. Format: "<listen host:port>=<intranet target>,..."
	if maps := s.tunnels.mapsFor(sess.User); len(maps) > 0 {
		parts := make([]string, len(maps))
		for i, m := range maps {
			parts[i] = fmt.Sprintf("%s=%s", net.JoinHostPort(tc.ProxyHost, strconv.Itoa(m.Port)), m.Target)
		}
		env = append(env, "AGENTBOX_INTRANET_MAPS="+strings.Join(parts, ","))
	}
	return env
}

// handleTunnelStatus reports the caller's own tunnel state for the UI: feature
// switch, proxy address, live connection facts and active port maps. Admins
// additionally see which users are online.
func (s *Server) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	u := reqUser(r)
	tc := s.cfg.GetTunnel()
	out := map[string]any{
		"enabled":   tc.Enabled,
		"proxy_up":  s.tunnels.proxyUp.Load(),
		"connected": false,
	}
	if tc.Enabled {
		out["proxy"] = net.JoinHostPort(tc.ProxyHost, portOf(tc.ProxyBind))
	}
	if ci, ok := s.tunnels.connInfoFor(u.Name); ok {
		out["connected"] = true
		out["since"] = ci.Since.UnixMilli()
		out["remote"] = ci.Remote
	}
	maps := s.tunnels.mapsFor(u.Name)
	views := make([]map[string]any, 0, len(maps))
	for _, m := range maps {
		views = append(views, map[string]any{
			"listen": net.JoinHostPort(tc.ProxyHost, strconv.Itoa(m.Port)),
			"target": m.Target,
		})
	}
	out["maps"] = views
	if u.Role == store.RoleAdmin {
		out["online_users"] = s.tunnels.onlineUsers()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- abox-link client downloads ---
//
// Pre-built abox-link binaries dropped into <data_dir>/abox-link/ are offered
// for download in the tunnel dialog, so users don't need a Go toolchain. The
// directory is optional; without it the UI shows build-from-source hints.

func (s *Server) clientDir() string { return filepath.Join(s.cfg.DataDir, "abox-link") }

func (s *Server) handleTunnelClients(w http.ResponseWriter, r *http.Request) {
	ents, err := os.ReadDir(s.clientDir())
	out := []map[string]any{}
	if err == nil {
		for _, e := range ents {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, map[string]any{"name": e.Name(), "size": fi.Size()})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTunnelClientGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// The name must be a plain file name inside the client dir — no traversal.
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, "invalid client name")
		return
	}
	path := filepath.Join(s.clientDir(), name)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		writeErr(w, http.StatusNotFound, "client binary not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}

// boundConn presents a *net.TCPAddr LocalAddr so go-socks5 can encode the
// CONNECT success reply; the underlying conn is the yamux tunnel stream.
type boundConn struct{ net.Conn }

func (boundConn) LocalAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4zero, Port: 0} }
func (boundConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero, Port: 0} }

func portOf(hostPort string) string {
	_, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	return port
}
