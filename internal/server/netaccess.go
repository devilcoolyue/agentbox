package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/dockerx"
	"agentbox/internal/netaccess"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/tunnel"
)

type networkReport struct {
	netaccess.Report
	Seen      time.Time
	Container string
}
type networkState struct {
	connectMu sync.Mutex
	mu        sync.Mutex
	policies  map[string]netaccess.Policy
	reports   map[string]networkReport
	ln        net.Listener
	bind      string
}

func (s *Server) networkSecret(sess store.Session) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.GetAuthToken()))
	fmt.Fprintf(mac, "network:%s:%s:%s", sess.ID, sess.User, sess.ContainerID)
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Server) networkPolicy(user string) (netaccess.Policy, error) {
	s.network.mu.Lock()
	defer s.network.mu.Unlock()
	return s.loadNetworkPolicy(user)
}
func (s *Server) loadNetworkPolicy(user string) (netaccess.Policy, error) {
	if s.network.policies == nil {
		s.network.policies = map[string]netaccess.Policy{}
	}
	if p, ok := s.network.policies[user]; ok {
		return p, nil
	}
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return netaccess.Policy{}, err
	}
	defer root.Close()
	raw, err := root.ReadAll(filepath.Join("users", user, "network.json"), 1<<20)
	var p netaccess.Policy
	if err != nil && !os.IsNotExist(err) {
		return p, err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &p); err != nil {
			return p, err
		}
	} else {
		p, _ = netaccess.Merge(p, nil)
	}
	s.network.policies[user] = p
	return p, nil
}
func (s *Server) saveNetworkRules(user string, rules []string) error {
	s.network.mu.Lock()
	defer s.network.mu.Unlock()
	old, err := s.loadNetworkPolicy(user)
	if err != nil {
		return err
	}
	p, err := netaccess.Merge(old, rules)
	if err != nil {
		return err
	}
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := filepath.Join("users", user)
	if err = root.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	if _, err = root.WriteFile(filepath.Join(dir, "network.json"), raw, safefs.WriteOptions{Mode: 0600}); err != nil {
		return err
	}
	s.network.policies[user] = p
	return nil
}
func (s *Server) applyNetwork() error {
	tc := s.cfg.GetTunnel()
	s.network.mu.Lock()
	defer s.network.mu.Unlock()
	if s.network.ln != nil && (!tc.Transparent || s.network.bind != tc.NetworkBind) {
		s.network.ln.Close()
		s.network.ln = nil
	}
	if !tc.Transparent || s.network.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", tc.NetworkBind)
	if err != nil {
		return fmt.Errorf("透明网络控制端口: %w", err)
	}
	s.network.ln = ln
	s.network.bind = tc.NetworkBind
	srv := &http.Server{Handler: s.admit(http.HandlerFunc(s.handleNetworkControl)), ReadHeaderTimeout: 10 * time.Second}
	release, ok := s.track(func() { srv.Close(); ln.Close() })
	if !ok {
		ln.Close()
		s.network.ln = nil
		return context.Canceled
	}
	if !s.spawn(func() {
		defer release()
		defer srv.Close()
		srv.Serve(ln)
		s.network.mu.Lock()
		if s.network.ln == ln {
			s.network.ln = nil
		}
		s.network.mu.Unlock()
	}) {
		release()
		ln.Close()
		s.network.ln = nil
		return context.Canceled
	}
	return nil
}
func (s *Server) handleNetworkControl(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	sess, found := s.store.Get(id)
	if !ok || !found || sess.ContainerID == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(s.networkSecret(sess))) != 1 {
		http.Error(w, "invalid workspace credential", 401)
		return
	}
	if _, err := s.sessionAccount(sess); err != nil {
		http.Error(w, "workspace account access revoked", 403)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/policy":
		p, err := s.networkPolicy(sess.User)
		if err != nil {
			http.Error(w, "policy unavailable", 503)
			return
		}
		writeJSON(w, 200, p)
	case r.Method == "POST" && r.URL.Path == "/status":
		var report netaccess.Report
		if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&report) != nil {
			http.Error(w, "invalid status", 400)
			return
		}
		s.network.mu.Lock()
		if s.network.reports == nil {
			s.network.reports = map[string]networkReport{}
		}
		s.network.reports[id] = networkReport{Report: report, Seen: time.Now(), Container: sess.ContainerID}
		s.network.mu.Unlock()
		w.WriteHeader(204)
	case r.Method == "CONNECT":
		conn, err := s.dialIntranet(r.Context(), sess.User, r.Host)
		if err != nil {
			http.Error(w, "intranet unavailable or denied", 502)
			return
		}
		defer conn.Close()
		s.spliceNetwork(w, r, conn)
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) dialIntranet(ctx context.Context, user, target string) (net.Conn, error) {
	tc := s.cfg.GetTunnel()
	if !tc.Enabled || !tc.Transparent {
		return nil, fmt.Errorf("transparent access disabled")
	}
	p, err := s.networkPolicy(user)
	if err != nil {
		return nil, err
	}
	target, err = p.Target(target)
	if err != nil || !p.Allows(target) {
		return nil, fmt.Errorf("target denied")
	}
	info, online := s.tunnels.connInfoFor(user)
	if !online || !info.Transparent {
		return nil, fmt.Errorf("transparent client offline")
	}
	sess := s.tunnels.session(user)
	if sess == nil || sess.IsClosed() {
		return nil, fmt.Errorf("tunnel offline")
	}
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	stream.SetDeadline(time.Now().Add(12 * time.Second))
	if err = tunnel.WriteConnect(stream, target); err != nil {
		stream.Close()
		return nil, err
	}
	var status [1]byte
	if _, err = io.ReadFull(stream, status[:]); err != nil || status[0] != tunnel.StatusOK {
		stream.Close()
		return nil, fmt.Errorf("target denied or unavailable")
	}
	stream.SetDeadline(time.Time{})
	return stream, nil
}
func (s *Server) spliceNetwork(w http.ResponseWriter, r *http.Request, up net.Conn) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unavailable", 500)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer down.Close()
	release, ok := s.track(func() { down.Close(); up.Close() })
	if !ok {
		return
	}
	defer release()
	if _, err = down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	if n := buf.Reader.Buffered(); n > 0 {
		if _, err = io.CopyN(up, buf, int64(n)); err != nil {
			return
		}
	}
	netaccess.Pipe(down, up)
}
func (s *Server) ensureNetwork(ctx context.Context, sess store.Session) error {
	tc := s.cfg.GetTunnel()
	if !tc.Transparent || !tc.Enabled {
		return nil
	}
	if err := s.applyNetwork(); err != nil {
		return err
	}
	cfg := netaccess.HelperConfig{Control: "http://" + net.JoinHostPort(tc.ProxyHost, portOf(tc.NetworkBind)), Session: sess.ID, Secret: s.networkSecret(sess)}
	if err := s.dock.EnsureNetwork(ctx, sess, tc.NetworkImage, cfg); err != nil {
		return err
	}
	if err := agent.SeedTransparentHint(sess.Agent, s.homeDir(sess), dockerx.AgentUID, dockerx.AgentGID); err != nil {
		log.Printf("seed transparent network hint %s: %v", sess.ID, err)
	}
	p, err := s.networkPolicy(sess.User)
	if err != nil {
		return err
	}
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		st := s.networkStatus(sess, p)
		if st.Ready {
			return nil
		}
		if st.Error != "" {
			return fmt.Errorf("透明网络未就绪: %s", st.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("透明网络初始化超时，请检查辅助镜像与控制端口")
		case <-ticker.C:
		}
	}
}

type workspaceNetworkView struct {
	Session string `json:"session"`
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Error   string `json:"error,omitempty"`
}

func (s *Server) networkStatus(sess store.Session, p netaccess.Policy) workspaceNetworkView {
	s.network.mu.Lock()
	report, ok := s.network.reports[sess.ID]
	s.network.mu.Unlock()
	v := workspaceNetworkView{Session: sess.ID, Name: sess.Name}
	if ok && report.Container == sess.ContainerID && time.Since(report.Seen) < 10*time.Second {
		v.Error = report.Error
		v.Ready = report.Error == "" && report.Revision == p.Revision
	} else {
		v.Error = ""
	}
	return v
}
func (s *Server) networkLoop() {
	for {
		if tc := s.cfg.GetTunnel(); tc.Transparent && tc.Enabled {
			for _, sess := range s.store.All() {
				if sess.Status != store.StatusRunning {
					continue
				}
				ctx, cancel := context.WithTimeout(s.workContext(), 15*time.Second)
				err := s.workspaces().RefreshNetwork(ctx, sess.ID)
				cancel()
				if err != nil && s.workContext().Err() == nil {
					s.network.mu.Lock()
					if s.network.reports == nil {
						s.network.reports = map[string]networkReport{}
					}
					s.network.reports[sess.ID] = networkReport{Report: netaccess.Report{Error: err.Error()}, Container: sess.ContainerID, Seen: time.Now()}
					s.network.mu.Unlock()
				}
			}
		}
		if !waitInterval(s.workContext(), 10*time.Second) {
			return
		}
	}
}

// bridgeWorkspace authenticates the source workspace independently of the
// account credential, since an account can be shared by multiple users.
func (s *Server) bridgeWorkspace(r *http.Request) (store.Session, bool) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || s.dock == nil {
		return store.Session{}, false
	}
	account, _, _ := parseProxyAuth(r.Header.Get("Proxy-Authorization"))
	var found store.Session
	for _, sess := range s.store.All() {
		if sess.AccountID != account || sess.Status != store.StatusRunning {
			continue
		}

		// Resolve actual Docker ownership, never trusting a caller-supplied user.
		for _, ownIP := range s.dock.ContainerIPs(r.Context(), sess.ContainerID) {
			if ownIP == ip {
				if found.ID != "" {
					return store.Session{}, false
				}
				found = sess
			}
		}
	}
	if found.ID == "" {
		return found, false
	}
	_, err = s.sessionAccount(found)
	return found, err == nil
}
func (s *Server) bridgeIntranet(r *http.Request, target string) (net.Conn, bool, error) {
	if !s.cfg.GetTunnel().Transparent {
		return nil, false, nil
	}
	sess, ok := s.bridgeWorkspace(r)
	if !ok {
		return nil, true, fmt.Errorf("cannot authenticate workspace source")
	}
	p, err := s.networkPolicy(sess.User)
	if err != nil {
		return nil, true, err
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return nil, true, err
	}
	if !p.Captures(host) {
		return nil, false, nil
	}
	conn, err := s.dialIntranet(r.Context(), sess.User, target)
	return conn, true, err
}

func (s *Server) handleNetworkProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		writeErr(w, 400, "invalid target")
		return
	}
	if strings.ContainsAny(req.Target, "\r\n") {
		writeErr(w, 400, "invalid target")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := s.dialIntranet(ctx, reqUser(r).Name, req.Target)
	if err != nil {
		writeErr(w, 502, "目标未放行、隧道离线或目标无法连接")
		return
	}
	conn.Close()
	writeJSON(w, 200, map[string]any{"ok": true, "elapsed_ms": time.Since(start).Milliseconds()})
}
