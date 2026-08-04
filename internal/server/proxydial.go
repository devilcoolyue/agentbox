// 出口代理的拨号层：把一个 config.Proxy 变成「拨到 host:port 的 TCP 连接」。
// 上层有两个消费者——容器侧的 HTTP 桥接（proxybridge.go）和服务端自己发官方
// 请求用的 http.Client（proxyClient）——它们共用这里的一套 socks5 / http
// CONNECT 实现，代理协议的差异不外泄。
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"time"

	xproxy "golang.org/x/net/proxy"

	"agentbox/internal/config"
)

// proxyDialTimeout caps the handshake with the proxy itself (TCP + SOCKS
// negotiation / CONNECT). The upstreams are residential exits, so it is
// deliberately generous.
const proxyDialTimeout = 20 * time.Second

// dialThrough opens a TCP connection to addr ("host:port") through p.
//
// Name resolution happens at the proxy, never here: the point of binding an
// account to an exit IP is that the provider sees only that IP, and resolving
// api.anthropic.com locally would leak a DNS query from the server's own
// resolver. SOCKS5 gets the hostname verbatim (socks5h semantics, which is what
// x/net/proxy does with a non-IP address); HTTP proxies get it in the CONNECT
// request line.
func dialThrough(ctx context.Context, p config.Proxy, addr string) (net.Conn, error) {
	if p.Disabled {
		return nil, fmt.Errorf("代理 %s 已停用", proxyLabel(p))
	}
	switch p.Scheme {
	case config.ProxySOCKS5:
		return dialSOCKS5(ctx, p, addr)
	case config.ProxyHTTP, config.ProxyHTTPS:
		return dialHTTPConnect(ctx, p, addr)
	default:
		return nil, fmt.Errorf("代理 %s 协议 %q 不支持", proxyLabel(p), p.Scheme)
	}
}

func dialSOCKS5(ctx context.Context, p config.Proxy, addr string) (net.Conn, error) {
	var auth *xproxy.Auth
	if p.Username != "" {
		auth = &xproxy.Auth{User: p.Username, Password: p.Password}
	}
	d, err := xproxy.SOCKS5("tcp", p.Endpoint(), auth, &net.Dialer{Timeout: proxyDialTimeout})
	if err != nil {
		return nil, fmt.Errorf("代理 %s: %w", proxyLabel(p), err)
	}
	// x/net's SOCKS5 dialer always implements ContextDialer; the assertion is
	// checked so a future refactor fails loudly instead of dropping the ctx.
	cd, ok := d.(xproxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("代理 %s: SOCKS5 拨号器不支持 context", proxyLabel(p))
	}
	conn, err := cd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("代理 %s: %w", proxyLabel(p), err)
	}
	return conn, nil
}

// dialHTTPConnect tunnels through an HTTP(S) proxy with a CONNECT request.
func dialHTTPConnect(ctx context.Context, p config.Proxy, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: proxyDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", p.Endpoint())
	if err != nil {
		return nil, fmt.Errorf("代理 %s: %w", proxyLabel(p), err)
	}
	if p.Scheme == config.ProxyHTTPS {
		tconn := tls.Client(conn, &tls.Config{ServerName: p.Host})
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("代理 %s TLS 握手失败: %w", proxyLabel(p), err)
		}
		conn = tconn
	}

	// The handshake has no timeout of its own once the socket is up; borrow the
	// caller's deadline so a silent proxy can't hang the exec forever.
	deadline := time.Now().Add(proxyDialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if p.Username != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(p.Username + ":" + p.Password))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("代理 %s: 发送 CONNECT 失败: %w", proxyLabel(p), err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("代理 %s: 读取 CONNECT 响应失败: %w", proxyLabel(p), err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("代理 %s: CONNECT 被拒绝 (HTTP %d)", proxyLabel(p), resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	// A proxy may pipeline bytes right behind the 200; those are already in br,
	// so hand the caller a conn that reads them first.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// bufferedConn replays bytes the CONNECT reader over-read before falling back
// to the socket.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// proxyLabel names a proxy in error messages without ever printing credentials.
func proxyLabel(p config.Proxy) string {
	if p.Name != "" && p.Name != p.Host {
		return p.Name + "(" + p.Endpoint() + ")"
	}
	return p.Endpoint()
}

// proxyTransport builds an http.Transport whose every connection goes through
// p. Tunnelling at the dial layer (rather than Transport.Proxy) means plain
// http:// and https:// targets take the same path, and a SOCKS5 upstream works
// even though Transport.Proxy would not accept one.
func proxyTransport(p config.Proxy) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialThrough(ctx, p, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// acctClient returns the HTTP client for talking to a provider on behalf of
// acct: through the bound proxy when there is one, direct otherwise.
//
// An account bound to a proxy never falls back to a direct client — that would
// hand the provider the server's own IP, which is the exact thing the binding
// exists to prevent. A broken binding surfaces as a failed request instead.
func (s *Server) acctClient(acct config.Account, timeout time.Duration) (*http.Client, error) {
	p, bound := s.cfg.AccountProxy(acct.ID)
	if !bound {
		return &http.Client{Timeout: timeout}, nil
	}
	if p.Disabled {
		return nil, fmt.Errorf("账号绑定的代理 %s 已停用或不存在，请在「IP 代理」里修复后重试", proxyLabel(p))
	}
	if !config.ValidProxyScheme(p.Scheme) {
		return nil, fmt.Errorf("账号绑定的代理 %s 协议无效", proxyLabel(p))
	}
	return &http.Client{Timeout: timeout, Transport: proxyTransport(p)}, nil
}
