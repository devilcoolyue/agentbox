// 受限出网（container.egress = "restricted"）。
//
// 账号出口代理本身只是注入 HTTP(S)_PROXY，任何不读这组变量的程序（或 curl
// --noproxy）都能从服务器自己的 IP 直连出去，还能连宿主机上的 sshd、数据库和
// 同网桥的其他工作空间。受限模式在每个工作空间的网络命名空间里装 iptables：
// 只放行 agentbox 自己给容器的监听（代理桥、隧道、网络控制、模型网关、用量
// 回传和临时桥接端口段），其余一律拒绝，入站新连接全部丢弃。
//
// 出网于是只剩代理桥一条路：绑了代理的账号照旧走自己的代理；没绑代理的账号
// 由桥接从服务器直连（与以前的出口 IP 相同），但只允许公网目标——回环、内网、
// 宿主机自身地址一概拒绝，否则桥接本身就成了访问宿主机服务的跳板。
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/netaccess"
	"agentbox/internal/store"
)

// egressState remembers which workspace namespaces already carry the current
// rules, keyed by container ID → "<StartedAt>|<rules hash>". Docker builds a
// fresh namespace on every container start, so a changed StartedAt means the
// rules are gone and must be reinstalled.
type egressState struct {
	mu     sync.Mutex
	locked map[string]string
}

func (s *Server) egressRestricted() bool {
	return s.cfg.GetContainer().EgressMode() == config.EgressRestricted
}

// workspaceNetwork is the workspace lifecycle network hook: the egress lock
// goes in first so nothing runs unrestricted, then the transparent helper.
func (s *Server) workspaceNetwork(ctx context.Context, sess store.Session) error {
	if err := s.ensureEgress(ctx, sess); err != nil {
		return err
	}
	return s.ensureNetwork(ctx, sess)
}

// ensureEgress installs (or refreshes) the restricted-egress rules. It runs
// under the workspace lifecycle lock on every start/attach and from the
// network loop, and is a single inspect when nothing changed.
func (s *Server) ensureEgress(ctx context.Context, sess store.Session) error {
	if !s.egressRestricted() || sess.ContainerID == "" || s.cfg.GetContainer().EgressExempt(sess.User) {
		return nil
	}
	p, err := s.egressPolicy(ctx)
	if err != nil {
		return err
	}
	v4, v6, err := p.Rules()
	if err != nil {
		return fmt.Errorf("受限出网: %w", err)
	}
	sum := sha256.Sum256([]byte(v4 + "\x00" + v6))
	fp := hex.EncodeToString(sum[:8])
	started, err := s.dock.StartedAt(ctx, sess.ContainerID)
	if err != nil {
		return err
	}
	s.egress.mu.Lock()
	done := s.egress.locked[sess.ContainerID] == started+"|"+fp
	s.egress.mu.Unlock()
	if done {
		return nil
	}
	started, err = s.dock.LockEgress(ctx, sess.ContainerID, s.cfg.GetTunnel().NetworkImage, v4, v6)
	if err != nil {
		return err
	}
	s.egress.mu.Lock()
	if s.egress.locked == nil {
		s.egress.locked = map[string]string{}
	}
	s.egress.locked[sess.ContainerID] = started + "|" + fp
	s.egress.mu.Unlock()
	return nil
}

// pruneEgress forgets containers that are no longer running workspaces.
func (s *Server) pruneEgress(running map[string]bool) {
	s.egress.mu.Lock()
	defer s.egress.mu.Unlock()
	for id := range s.egress.locked {
		if !running[id] {
			delete(s.egress.locked, id)
		}
	}
}

// egressPolicy lists every host endpoint agentbox advertises to workspaces.
func (s *Server) egressPolicy(ctx context.Context) (netaccess.EgressPolicy, error) {
	var p netaccess.EgressPolicy
	add := func(name, host, bind string) error {
		addr, err := egressHost(ctx, host)
		if err != nil {
			return fmt.Errorf("受限出网: %s 的容器访问地址 %q 不可用: %w", name, host, err)
		}
		port, err := strconv.Atoi(portOf(bind))
		if err != nil {
			return fmt.Errorf("受限出网: %s 监听地址 %q 无效", name, bind)
		}
		p.Hosts = append(p.Hosts, addr)
		p.Ports = append(p.Ports, port)
		return nil
	}
	pb := s.cfg.GetProxyBridge()
	if err := add("proxy_bridge", pb.Host, pb.Bind); err != nil {
		return p, err
	}
	r := pb.DynamicPortRange()
	p.RangeLow, p.RangeHigh = r.Low, r.High
	if tc := s.cfg.GetTunnel(); tc.Enabled {
		if err := add("tunnel", tc.ProxyHost, tc.ProxyBind); err != nil {
			return p, err
		}
		if tc.Transparent {
			if err := add("tunnel.network_bind", tc.ProxyHost, tc.NetworkBind); err != nil {
				return p, err
			}
		}
	}
	gw := s.cfg.GetModelGateway()
	if err := add("model_gateway", gw.Host, gw.Bind); err != nil {
		return p, err
	}
	if ut := s.cfg.GetUsageTelemetry(); !ut.Disabled {
		if err := add("usage_telemetry", ut.Host, ut.Bind); err != nil {
			return p, err
		}
	}
	return p, nil
}

func egressHost(ctx context.Context, host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		a = a.Unmap()
		if !a.Is4() || a.IsUnspecified() {
			return netip.Addr{}, errors.New("需要具体的 IPv4 地址")
		}
		return a, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("无法解析为 IPv4: %v", err)
	}
	return addrs[0].Unmap(), nil
}

// directProxy is the bridge route for an account without a bound proxy under
// restricted egress: the server dials public targets itself. It never comes
// from config, so dialThrough is the only place that has to know about it.
var directProxy = config.Proxy{Name: "服务器直连", Scheme: directScheme}

const directScheme = "direct"

// nonPublic covers special-purpose ranges netip's predicates do not: CGNAT,
// documentation/benchmark nets, reserved space, and IPv6 prefixes that embed
// an IPv4 address (NAT64, 6to4) and could be used to reach a private one.
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32", "2002::/16", "fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// publicTarget reports whether a may be dialed directly from the server on a
// workspace's behalf: a global unicast address that is neither private,
// special-purpose nor one of the host's own interface addresses.
func publicTarget(a netip.Addr, own map[netip.Addr]bool) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || own[a] {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func hostAddrs() map[netip.Addr]bool {
	own := map[netip.Addr]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			own[p.Addr().Unmap()] = true
		}
	}
	return own
}

// dialPublic resolves addr on the server and dials the first public address.
// Dialing the checked IP (not the name) keeps DNS rebinding from swapping in a
// private address between the check and the connect.
func dialPublic(ctx context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if a, e := netip.ParseAddr(host); e == nil {
		ips = []netip.Addr{a}
	} else if ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	own := hostAddrs()
	d := net.Dialer{Timeout: 15 * time.Second}
	last := fmt.Errorf("受限出网: %s 不是公网地址", host)
	for _, ip := range ips {
		if !publicTarget(ip, own) {
			continue
		}
		c, e := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	return nil, last
}

// listenDynamic binds a per-request bridge listener inside
// proxy_bridge.dynamic_ports, the range restricted egress lets workspaces
// reach. Starting at a random offset keeps concurrent grants from colliding.
func (s *Server) listenDynamic(host string) (net.Listener, error) {
	r := s.cfg.GetProxyBridge().DynamicPortRange()
	n := r.High - r.Low + 1
	start := rand.IntN(n)
	var last error
	for i := range n {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(r.Low+(start+i)%n)))
		if err == nil {
			return ln, nil
		}
		last = err
	}
	return nil, fmt.Errorf("proxy_bridge.dynamic_ports %s 没有空闲端口: %w", r, last)
}

func logEgress(sess store.Session, err error) {
	log.Printf("restricted egress %s (%s): %v", sess.ID, sess.Name, err)
}
