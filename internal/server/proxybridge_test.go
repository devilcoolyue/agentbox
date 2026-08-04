package server

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	socks5 "github.com/things-go/go-socks5"

	"agentbox/internal/config"
)

// startUpstreamSOCKS runs a real SOCKS5 server with user/password auth, playing
// the part of a bought proxy exit.
func startUpstreamSOCKS(t *testing.T, user, pass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := socks5.NewServer(socks5.WithAuthMethods([]socks5.Authenticator{
		socks5.UserPassAuthenticator{Credentials: socks5.StaticCredentials{user: pass}},
	}))
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String()
}

// startBridge boots the account proxy bridge on a loopback port and returns the
// proxy URL a container would be handed.
func startBridge(t *testing.T, s *Server) *url.URL {
	t.Helper()
	if err := s.applyProxyBridge(); err != nil {
		t.Fatalf("applyProxyBridge: %v", err)
	}
	t.Cleanup(func() {
		s.bridgeMu.Lock()
		if s.bridgeLn != nil {
			_ = s.bridgeLn.Close()
		}
		s.bridgeMu.Unlock()
	})
	if !s.bridgeUp.Load() {
		t.Fatal("桥接没有起来")
	}
	u, err := url.Parse("http://" + s.bridgeLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("claude-1", s.proxySecret("claude-1"))
	return u
}

// bridgeTestServer wires an account bound to an upstream SOCKS5 proxy, with the
// bridge listening on an ephemeral loopback port.
func bridgeTestServer(t *testing.T, upstream string, user, pass string) *Server {
	t.Helper()
	host, portStr, err := net.SplitHostPort(upstream)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg: &config.Config{
			AuthToken: "a-long-enough-token",
			Accounts: []config.Account{
				{ID: "claude-1", Type: "claude", Label: "A", ProxyID: "px1"},
			},
			Proxies: []config.Proxy{
				{ID: "px1", Name: "上游", Scheme: config.ProxySOCKS5,
					Host: host, Port: port, Username: user, Password: pass},
			},
			ProxyBridge: config.ProxyBridgeConfig{Bind: "127.0.0.1:0", Host: "127.0.0.1"},
		},
	}
	return s
}

// 端到端：容器 → 桥接（HTTP 代理）→ 上游 SOCKS5 → 目标。这条链路是整个功能的
// 全部意义所在——claude 那个 Node 客户端只会说 HTTP 代理，socks5 那一段必须由
// 服务端补上。
func TestProxyBridgeEndToEnd(t *testing.T) {
	upstream := startUpstreamSOCKS(t, "pxuser", "pxpass")
	s := bridgeTestServer(t, upstream, "pxuser", "pxpass")
	proxyURL := startBridge(t, s)

	t.Run("CONNECT 隧道（https 目标）", func(t *testing.T) {
		target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "OK-TLS")
		}))
		defer target.Close()
		cl := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy:           http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
		if body := getBody(t, cl, target.URL); body != "OK-TLS" {
			t.Errorf("body = %q, want OK-TLS", body)
		}
	})

	t.Run("绝对地址转发（http 目标）", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "OK-PLAIN")
		}))
		defer target.Close()
		cl := &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		}
		if body := getBody(t, cl, target.URL); body != "OK-PLAIN" {
			t.Errorf("body = %q, want OK-PLAIN", body)
		}
	})

	t.Run("凭证不对的容器进不来", func(t *testing.T) {
		bad := *proxyURL
		bad.User = url.UserPassword("claude-1", "wrong-secret")
		cl := &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyURL(&bad)},
		}
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer target.Close()
		resp, err := cl.Get(target.URL)
		if err != nil {
			return // 传输层直接拒绝也算拦住了
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusProxyAuthRequired {
			t.Errorf("status = %d, want 407", resp.StatusCode)
		}
	})
}

// 上游认证信息错误时必须报错，而不是绕过代理直接连上目标——那等于悄悄泄露
// 服务器真实出口 IP。
func TestProxyBridgeFailsClosedOnBadUpstreamAuth(t *testing.T) {
	upstream := startUpstreamSOCKS(t, "pxuser", "pxpass")
	s := bridgeTestServer(t, upstream, "pxuser", "wrong-password")
	proxyURL := startBridge(t, s)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "LEAKED")
	}))
	defer target.Close()

	cl := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	resp, err := cl.Get(target.URL)
	if err != nil {
		return // 连不上就对了
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) == "LEAKED" {
		t.Fatal("上游代理认证失败时竟然直连到了目标——真实 IP 泄露")
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// 代理池清空后桥接自己收摊，不占着端口。
func TestApplyProxyBridgeStopsWhenPoolEmpties(t *testing.T) {
	upstream := startUpstreamSOCKS(t, "u", "p")
	s := bridgeTestServer(t, upstream, "u", "p")
	startBridge(t, s)

	s.cfg.Proxies = nil
	if err := s.applyProxyBridge(); err != nil {
		t.Fatal(err)
	}
	if s.bridgeUp.Load() {
		t.Error("代理池空了，桥接还在监听")
	}
}

func getBody(t *testing.T, cl *http.Client, target string) string {
	t.Helper()
	resp, err := cl.Get(target)
	if err != nil {
		t.Fatalf("get %s: %v", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
