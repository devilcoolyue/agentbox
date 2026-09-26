package netaccess

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentbox/internal/tunnel"
	"golang.org/x/net/dns/dnsmessage"
)

// HelperConfig contains only the session-scoped control credential, never an
// account credential. It is supplied to the root-owned network sidecar.
type HelperConfig struct{ Control, Session, Secret string }
type Report struct {
	Revision string `json:"revision"`
	Error    string `json:"error,omitempty"`
}
type Helper struct {
	cfg       HelperConfig
	client    *http.Client
	mu        sync.RWMutex
	policy    Policy
	ready     bool
	errorText string
	upstream  string
	networks  []netip.Prefix
}

func RunHelper(ctx context.Context, cfg HelperConfig) error {
	u, err := url.Parse(cfg.Control)
	if err != nil || u.Scheme != "http" || u.Host == "" || cfg.Session == "" || cfg.Secret == "" {
		return fmt.Errorf("invalid network helper configuration")
	}
	if err := platformCheck(); err != nil {
		return err
	}
	h := &Helper{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}}
	raw, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == "nameserver" {
			h.upstream = net.JoinHostPort(f[1], "53")
			break
		}
	}
	if h.upstream == "" {
		return fmt.Errorf("no upstream DNS server")
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if p, e := netip.ParsePrefix(a.String()); e == nil && !p.Addr().IsLoopback() {
			h.networks = append(h.networks, p.Masked())
		}
	}
	// Rules outlive this process. Restarting or losing control never restores a
	// direct path for already captured destinations.
	if err := installCapture(ctx); err != nil {
		return err
	}
	tcp, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", TCPPort))
	if err != nil {
		return err
	}
	defer tcp.Close()
	dnsTCP, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", DNSPort))
	if err != nil {
		return err
	}
	defer dnsTCP.Close()
	dnsUDP, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", DNSPort))
	if err != nil {
		return err
	}
	defer dnsUDP.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	launch := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	launch(func() { <-runCtx.Done(); tcp.Close(); dnsTCP.Close(); dnsUDP.Close() })
	launch(func() { h.poll(runCtx) })
	// Cap concurrent handlers: an agent cannot create unbounded DNS goroutines.
	slots := make(chan struct{}, 256)
	serve := func(ln net.Listener, handle func(context.Context, net.Conn)) {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				launch(func() {
					defer func() { <-slots }()
					defer c.Close()
					stop := context.AfterFunc(runCtx, func() { c.Close() })
					defer stop()
					handle(runCtx, c)
				})
			default:
				c.Close()
			}
		}
	}
	launch(func() { serve(tcp, h.handleTCP) })
	launch(func() { serve(dnsTCP, h.handleDNSTCP) })
	launch(func() {
		buf := make([]byte, 65535)
		for {
			n, addr, e := dnsUDP.ReadFrom(buf)
			if e != nil {
				return
			}
			packet := append([]byte(nil), buf[:n]...)
			select {
			case slots <- struct{}{}:
				launch(func() {
					defer func() { <-slots }()
					if out, e := h.answerDNS(runCtx, packet); e == nil {
						dnsUDP.WriteTo(out, addr)
					}
				})
			default:
			}
		}
	})
	<-runCtx.Done()
	wg.Wait()
	return nil
}
func (h *Helper) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.cfg.Control+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(h.cfg.Session, h.cfg.Secret)
	return h.client.Do(req)
}
func (h *Helper) poll(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		err := h.refresh(ctx)
		if err != nil {
			log.Printf("network policy: %v", err)
		}
		h.mu.RLock()
		report := Report{Revision: h.policy.Revision, Error: h.errorText}
		h.mu.RUnlock()
		if err != nil {
			report.Error = err.Error()
		}
		raw, _ := json.Marshal(report)
		if resp, e := h.request(ctx, "POST", "/status", strings.NewReader(string(raw))); e == nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (h *Helper) refresh(ctx context.Context) error {
	resp, err := h.request(ctx, "GET", "/policy", nil)
	if err != nil {
		return fmt.Errorf("control unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("control returned %d", resp.StatusCode)
	}
	var p Policy
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p); err != nil {
		return err
	}
	if err = p.CheckConflicts(h.networks); err != nil {
		h.mu.Lock()
		h.ready = false
		h.errorText = err.Error()
		h.mu.Unlock()
		return err
	}
	h.mu.RLock()
	changed := !h.ready || p.Revision != h.policy.Revision
	h.mu.RUnlock()
	if changed {
		if err = installBlocks(ctx, p); err != nil {
			h.mu.Lock()
			h.ready = false
			h.errorText = err.Error()
			h.mu.Unlock()
			return err
		}
		h.mu.Lock()
		h.policy = p
		h.ready = true
		h.errorText = ""
		h.mu.Unlock()
	}
	return nil
}
func (h *Helper) handleTCP(ctx context.Context, c net.Conn) {
	target, err := originalDestination(c)
	if err != nil {
		return
	}
	h.mu.RLock()
	p, ready := h.policy, h.ready
	h.mu.RUnlock()
	if !ready {
		return
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return
	}
	// A direct connection to the helper listener is not a redirected flow.
	// Never dial it back into ourselves (unbounded recursive proxy chains).
	if ip, e := netip.ParseAddr(host); e != nil || ip.IsLoopback() {
		return
	}
	var up net.Conn
	if p.Captures(host) {
		target, err = p.Target(target)
		if err == nil {
			up, err = DialControl(ctx, h.cfg, target)
		}
	} else {
		up, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", target)
	}
	if err != nil {
		return
	}
	defer up.Close()
	stop := context.AfterFunc(ctx, func() { up.Close() })
	defer stop()
	Pipe(c, up)
}

// DialControl opens an authenticated CONNECT without consulting proxy env.
func DialControl(ctx context.Context, cfg HelperConfig, target string) (net.Conn, error) {
	u, err := url.Parse(cfg.Control)
	if err != nil {
		return nil, err
	}
	c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	req := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
	req.SetBasicAuth(cfg.Session, cfg.Secret)
	if err = req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	reader := bufio.NewReader(c)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		c.Close()
		return nil, fmt.Errorf("intranet connect: %d", resp.StatusCode)
	}
	c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferedConn) CloseWrite() error {
	if x, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return x.CloseWrite()
	}
	return nil
}

// Pipe shares half-close semantics with the client side of the tunnel.
func Pipe(a, b net.Conn) { tunnel.Pipe(a, b) }

func (h *Helper) handleDNSTCP(ctx context.Context, c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var length [2]byte
	if _, err := io.ReadFull(c, length[:]); err != nil {
		return
	}
	packet := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(c, packet); err != nil {
		return
	}
	out, err := h.answerDNS(ctx, packet)
	if err != nil {
		return
	}
	binary.BigEndian.PutUint16(length[:], uint16(len(out)))
	c.Write(length[:])
	c.Write(out)
}
func (h *Helper) answerDNS(ctx context.Context, packet []byte) ([]byte, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(packet); err != nil {
		return nil, err
	}
	if msg.Response || len(msg.Questions) != 1 {
		return nil, fmt.Errorf("unsupported DNS question")
	}
	q := msg.Questions[0]
	host := strings.TrimSuffix(strings.ToLower(q.Name.String()), ".")
	h.mu.RLock()
	fake, local := h.policy.Domains[host]
	ready := h.ready
	h.mu.RUnlock()
	if !ready || local {
		out := dnsmessage.Message{Header: dnsmessage.Header{ID: msg.ID, Response: true, RecursionDesired: msg.RecursionDesired, RecursionAvailable: true}, Questions: msg.Questions}
		if !ready {
			out.RCode = dnsmessage.RCodeServerFailure
		} else if q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET {
			addr, err := netip.ParseAddr(fake)
			if err != nil {
				return nil, err
			}
			out.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 30}, Body: &dnsmessage.AResource{A: addr.As4()}}}
		}
		return out.Pack()
	}
	// Use TCP upstream: it handles truncated DNS answers and is independent of
	// the intercepted UDP socket. Root helper traffic bypasses capture rules.
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", h.upstream)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(packet)))
	if _, err = conn.Write(append(length[:], packet...)); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(conn, length[:]); err != nil {
		return nil, err
	}
	out := make([]byte, binary.BigEndian.Uint16(length[:]))
	_, err = io.ReadFull(conn, out)
	return out, err
}

func runIPTables(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "iptables", append([]string{"-w", "5"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("network rules: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
func ensureChain(ctx context.Context, table, chain string) error {
	if err := runIPTables(ctx, "-t", table, "-S", chain); err == nil {
		return nil
	}
	return runIPTables(ctx, "-t", table, "-N", chain)
}
func installCapture(ctx context.Context) error {
	if err := ensureChain(ctx, "nat", "ABX_NET"); err != nil {
		return err
	}
	rules := "*nat\n-F ABX_NET\n-A ABX_NET -p udp --dport 53 -j REDIRECT --to-ports " + strconv.Itoa(DNSPort) + "\n-A ABX_NET -p tcp --dport 53 -j REDIRECT --to-ports " + strconv.Itoa(DNSPort) + "\n-A ABX_NET -d 127.0.0.0/8 -j RETURN\n-A ABX_NET -p tcp -j REDIRECT --to-ports " + strconv.Itoa(TCPPort) + "\nCOMMIT\n"
	if err := restoreRules(ctx, rules); err != nil {
		return err
	}
	if err := runIPTables(ctx, "-t", "nat", "-C", "OUTPUT", "-m", "owner", "--uid-owner", "1000", "-j", "ABX_NET"); err != nil {
		return runIPTables(ctx, "-t", "nat", "-I", "OUTPUT", "1", "-m", "owner", "--uid-owner", "1000", "-j", "ABX_NET")
	}
	return nil
}
func installBlocks(ctx context.Context, p Policy) error {
	if err := ensureChain(ctx, "filter", "ABX_BLOCK"); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("*filter\n-F ABX_BLOCK\n")
	for _, r := range append(append([]string{}, p.Routes...), FakeCIDR) {
		if _, e := netip.ParseAddr(r); e != nil {
			if _, e = netip.ParsePrefix(r); e != nil {
				continue
			}
		}
		b.WriteString("-A ABX_BLOCK -d " + r + " ! -p tcp -j REJECT\n")
	}
	b.WriteString("COMMIT\n")
	if err := restoreRules(ctx, b.String()); err != nil {
		return err
	}
	if err := runIPTables(ctx, "-t", "filter", "-C", "OUTPUT", "-m", "owner", "--uid-owner", "1000", "-j", "ABX_BLOCK"); err != nil {
		return runIPTables(ctx, "-t", "filter", "-I", "OUTPUT", "1", "-m", "owner", "--uid-owner", "1000", "-j", "ABX_BLOCK")
	}
	return nil
}
func restoreRules(ctx context.Context, rules string) error {
	cmd := exec.CommandContext(ctx, "iptables-restore", "-w", "5", "--noflush")
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("network rules: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
