package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// unboundTestServer has one account with no bound proxy and an empty pool.
func unboundTestServer(egress string) *Server {
	return &Server{cfg: &config.Config{
		AuthToken:   "a-long-enough-token",
		Accounts:    []config.Account{{ID: "claude-1", Type: "claude", Label: "A"}},
		Container:   config.ContainerLimits{Network: "bridge", Egress: egress},
		ProxyBridge: config.ProxyBridgeConfig{Bind: "127.0.0.1:0", Host: "127.0.0.1"},
	}}
}

func TestPublicTarget(t *testing.T) {
	own := map[netip.Addr]bool{netip.MustParseAddr("154.219.123.10"): true}
	cases := map[string]bool{
		"1.1.1.1":               true,
		"160.79.104.10":         true,
		"2606:4700::1111":       true,
		"154.219.123.10":        false, // 宿主机自己的公网 IP：sshd 等就在这上面
		"127.0.0.1":             false,
		"10.1.2.3":              false,
		"172.17.0.1":            false,
		"192.168.1.1":           false,
		"169.254.169.254":       false, // 云厂商元数据
		"100.64.0.1":            false,
		"0.0.0.0":               false,
		"::1":                   false,
		"fe80::1":               false,
		"fd00::1":               false,
		"::ffff:127.0.0.1":      false,
		"64:ff9b::a00:1":        false, // NAT64 包着 10.0.0.1
		"2002:a00:1::1":         false, // 6to4 包着 10.0.0.1
		"224.0.0.1":             false,
		"255.255.255.255":       false,
		"198.18.0.1":            false,
		"::ffff:154.219.123.10": false,
	}
	for in, want := range cases {
		if got := publicTarget(netip.MustParseAddr(in), own); got != want {
			t.Errorf("publicTarget(%s) = %v, want %v", in, got, want)
		}
	}
}

// 受限出网下未绑代理的账号经桥接出网，但桥接不能被用来连回本机/内网服务。
func TestBridgeDirectRefusesPrivateTargets(t *testing.T) {
	s := unboundTestServer(config.EgressRestricted)
	proxyURL := startBridge(t, s)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "LEAKED")
	}))
	defer target.Close()
	t.Run("absolute-form", func(t *testing.T) {
		cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
		resp, err := cl.Get(target.URL)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) == "LEAKED" {
			t.Fatal("桥接替工作空间连到了回环地址上的服务")
		}
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
	})
	t.Run("CONNECT", func(t *testing.T) {
		if code := rawConnect(t, proxyURL, strings.TrimPrefix(target.URL, "http://")); code != http.StatusBadGateway {
			t.Fatalf("CONNECT status = %d, want 502", code)
		}
	})
}

func rawConnect(t *testing.T, proxyURL *url.URL, target string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyURL.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req, _ := http.NewRequest(http.MethodConnect, "http://"+target, nil)
	req.Host = target
	req.URL = &url.URL{Host: target}
	pass, _ := proxyURL.User.Password()
	req.SetBasicAuth(proxyURL.User.Username(), pass)
	req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
	req.Header.Del("Authorization")
	if err = req.Write(c); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	f := strings.Fields(string(buf[:n]))
	if len(f) < 2 {
		t.Fatalf("bad CONNECT reply %q", buf[:n])
	}
	code, _ := strconv.Atoi(f[1])
	return code
}

// 开放模式下未绑代理的账号行为不变：桥接不替它出网。
func TestBridgeUnboundOpenStillRefused(t *testing.T) {
	s := unboundTestServer("")
	// An empty pool keeps the bridge down in open mode; force it up to probe auth.
	s.cfg.Proxies = []config.Proxy{{ID: "px1", Scheme: config.ProxySOCKS5, Host: "127.0.0.1", Port: 1}}
	proxyURL := startBridge(t, s)
	if code := rawConnect(t, proxyURL, "1.1.1.1:443"); code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
}

func TestRestrictedBridgeStaysUpWithoutPool(t *testing.T) {
	s := unboundTestServer(config.EgressRestricted)
	startBridge(t, s)
	s.cfg.Container.Egress = config.EgressOpen
	if err := s.applyProxyBridge(); err != nil {
		t.Fatal(err)
	}
	if s.bridgeUp.Load() {
		t.Error("开放模式且代理池为空，桥接应停止")
	}
}

func TestProxyEnvForUnboundAccount(t *testing.T) {
	sess := store.Session{ID: "s1", AccountID: "claude-1"}
	if env := unboundTestServer("").proxyEnvList(sess); env != nil {
		t.Fatalf("open mode injected proxy env for an unbound account: %v", env)
	}
	env := unboundTestServer(config.EgressRestricted).proxyEnvList(sess)
	if len(env) != 8 || !strings.HasPrefix(env[1], "HTTPS_PROXY=http://claude-1:") {
		t.Fatalf("restricted mode env = %v", env)
	}
}

func TestListenDynamicStaysInRange(t *testing.T) {
	// A fixed range can be taken on a busy CI runner (it may sit inside the
	// kernel's ephemeral range); a port the kernel just handed out is free.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	s := &Server{cfg: &config.Config{ProxyBridge: config.ProxyBridgeConfig{DynamicPorts: fmt.Sprintf("%d-%d", free, free)}}}
	ln, err := s.listenDynamic("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if port := ln.Addr().(*net.TCPAddr).Port; port != free {
		t.Fatalf("port %d outside range %d-%d", port, free, free)
	}
	if extra, err := s.listenDynamic("127.0.0.1"); err == nil {
		extra.Close()
		t.Fatal("range exhausted but listen succeeded")
	}
}

func TestEgressPolicyCoversAdvertisedEndpoints(t *testing.T) {
	s := &Server{cfg: &config.Config{
		Container: config.ContainerLimits{Network: "bridge", Egress: config.EgressRestricted},
		Tunnel:    config.TunnelConfig{Enabled: true, Transparent: true, ProxyBind: "172.17.0.1:1080"},
	}}
	p, err := s.egressPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v4, _, err := p.Rules()
	if err != nil {
		t.Fatal(err)
	}
	want := "-d 172.17.0.1/32 -p tcp -m multiport --dports 1080,1081,1082,1083,1084,30000:30999 -j RETURN"
	if !strings.Contains(v4, want) {
		t.Fatalf("rules missing %q:\n%s", want, v4)
	}
	s.cfg.ProxyBridge.Host = "0.0.0.0"
	if _, err = s.egressPolicy(t.Context()); err == nil {
		t.Fatal("wildcard advertised host accepted")
	}
}

// unbound_egress=none: an account without a proxy gets no route at all, so no
// workspace can learn the server's own IP through an echo service.
func TestBridgeUnboundNoneRefusesEverything(t *testing.T) {
	s := unboundTestServer(config.EgressRestricted)
	s.cfg.Container.UnboundEgress = config.UnboundNone
	proxyURL := startBridge(t, s)
	if code := rawConnect(t, proxyURL, "1.1.1.1:443"); code != http.StatusBadGateway {
		t.Fatalf("CONNECT status = %d, want 502", code)
	}
	// The env is still injected so tools fail fast with the bridge's message.
	if env := s.proxyEnvList(store.Session{ID: "s1", AccountID: "claude-1"}); len(env) != 8 {
		t.Fatalf("env = %v", env)
	}
}
