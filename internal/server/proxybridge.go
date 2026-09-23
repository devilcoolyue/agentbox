// 账号出口代理的容器侧桥接。
//
// 代理池里绝大多数是 SOCKS5，而容器里跑的两个 CLI 只有一个认它：codex 是 Rust
// reqwest，认 socks5://；claude 是 Node/undici，HTTPS_PROXY 只认 http(s)://，
// 塞个 socks5:// 进去它直接忽略、照原样打官方接口——IP 一点没换，还看不出问题。
// 所以这里在宿主机 docker 网桥上起一个普通 HTTP 代理，容器只跟它说 HTTP 代理
// 协议，socks5 那一段由服务端自己走（proxydial.go）。
//
// 谁能用哪个上游，由代理认证决定：注入容器的 URL 形如
// http://<账号ID>:<密钥>@172.17.0.1:1081，桥接按用户名查账号绑定的代理。密钥是
// 服务端 auth_token 对账号 ID 的 HMAC，不落盘也不用同步；没有它，同一台机器上
// 任何一个容器都能白嫖别人账号的出口 IP。
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// The proxy variables injected into a session's execs. Named constants because
// the terminal mirrors them into tmux and has to unset them again when an
// account's proxy binding goes away (see tmuxEnvSync).
//
// Both cases are injected on purpose: curl reads lowercase `http_proxy`, Node
// and Go read the uppercase spellings, and getting it wrong fails silently by
// sending traffic out the server's own IP.
var proxyEnvNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

// proxyEnvList returns the proxy env for a session whose account is bound to an
// IP proxy. Unlike the intranet tunnel — which deliberately avoids a global
// proxy — this one IS global: the requirement is that everything the account
// does leaves through its own exit IP, and a per-tool opt-in would be one
// forgotten call away from leaking.
//
// A bound-but-unusable proxy still gets the env. Injecting nothing would route
// the account's official requests out of the server's real IP, which is the
// failure this feature exists to prevent; a hard error is the safer symptom.
func (s *Server) proxyEnvList(sess store.Session) []string {
	if _, bound := s.cfg.AccountProxy(sess.AccountID); !bound {
		return nil
	}
	pb := s.cfg.GetProxyBridge()
	host := pb.Host
	if host == "" {
		return nil // 无法拼出容器可达的地址，注入了也只会全面失败
	}
	url := fmt.Sprintf("http://%s:%s@%s", sess.AccountID, s.proxySecret(sess.AccountID),
		net.JoinHostPort(host, portOf(pb.Bind)))
	// 本机与网桥网关本身不走代理：隧道的 SOCKS 端口和端口映射都挂在网关上，
	// 绕一圈回代理只会自环。
	noProxy := "localhost,127.0.0.1,::1," + host
	return []string{
		"HTTP_PROXY=" + url, "HTTPS_PROXY=" + url, "ALL_PROXY=" + url, "NO_PROXY=" + noProxy,
		"http_proxy=" + url, "https_proxy=" + url, "all_proxy=" + url, "no_proxy=" + noProxy,
	}
}

// proxySecret derives an account's bridge password from the server auth token.
// Deriving beats storing: no extra state to persist, and rotating auth_token
// rotates every proxy credential with it (already-running execs keep the old
// value until their next exec, which is the same staleness the account env has).
func (s *Server) proxySecret(acctID string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.GetAuthToken()))
	mac.Write([]byte("proxy-bridge:" + acctID))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// applyProxyBridge reconciles the running bridge listener with the current
// config: it starts when the pool is non-empty, stops when the last proxy is
// deleted, and rebinds on a bind-address change. Called at boot and after every
// proxy/settings mutation, so the feature is hot — no restart needed. A bind
// failure is returned but non-fatal: the caller logs it and the server keeps
// serving without the bridge.
func (s *Server) applyProxyBridge() error {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	pb := s.cfg.GetProxyBridge()
	want := len(s.cfg.ProxyList()) > 0
	if s.bridgeLn != nil && (!want || s.bridgeBind != pb.Bind) {
		_ = s.bridgeLn.Close()
		s.bridgeLn = nil
		s.bridgeUp.Store(false)
		log.Printf("account proxy bridge on %s stopped", s.bridgeBind)
	}
	if !want || s.bridgeLn != nil {
		return nil
	}
	ln, err := net.Listen("tcp", pb.Bind)
	if err != nil {
		return fmt.Errorf("bind proxy bridge on %s: %w", pb.Bind, err)
	}
	s.bridgeLn = ln
	s.bridgeBind = pb.Bind
	s.bridgeUp.Store(true)
	log.Printf("account proxy bridge listening on %s", ln.Addr())
	srv := &http.Server{
		Handler:           s.admit(http.HandlerFunc(s.serveProxyBridge)),
		ReadHeaderTimeout: 15 * time.Second,
	}
	release, ok := s.track(func() { _ = srv.Close(); _ = ln.Close() })
	if !ok {
		return context.Canceled
	}
	if !s.spawn(func() {
		defer release()
		defer srv.Close()
		err := srv.Serve(ln)
		// Only report down if we are still the current listener — a rebind
		// closes us on purpose and has already flipped the state itself.
		s.bridgeMu.Lock()
		if s.bridgeLn == ln {
			s.bridgeLn = nil
			s.bridgeUp.Store(false)
			log.Printf("proxy bridge serve stopped: %v", err)
		}
		s.bridgeMu.Unlock()
	}) {
		release()
		_ = srv.Close()
		_ = ln.Close()
	}
	return nil
}

// serveProxyBridge handles both shapes a client uses an HTTP proxy in: CONNECT
// for TLS (which is what the model APIs are), and absolute-form requests for
// plain http:// targets.
func (s *Server) serveProxyBridge(w http.ResponseWriter, r *http.Request) {
	p, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodConnect {
		s.bridgeConnect(w, r, p)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "agentbox proxy bridge: absolute-form request required", http.StatusBadRequest)
		return
	}
	s.bridgeForward(w, r, p)
}

// bridgeAuth resolves the calling account from Proxy-Authorization and returns
// the proxy it is bound to. Anything unresolvable is answered here.
func (s *Server) bridgeAuth(w http.ResponseWriter, r *http.Request) (config.Proxy, bool) {
	user, pass, ok := parseProxyAuth(r.Header.Get("Proxy-Authorization"))
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="agentbox"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return config.Proxy{}, false
	}
	acct, found := s.cfg.Account(user)
	// Compare regardless of whether the account exists, and only then branch, so
	// the reply time does not tell an unknown id from a wrong secret.
	want := s.proxySecret(user)
	match := subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1
	if !found || !match {
		w.Header().Set("Proxy-Authenticate", `Basic realm="agentbox"`)
		http.Error(w, "proxy authentication failed", http.StatusProxyAuthRequired)
		return config.Proxy{}, false
	}
	p, bound := s.cfg.AccountProxy(acct.ID)
	if !bound {
		// The env is injected per exec; an admin can unbind mid-session and the
		// container keeps using the old env until its next exec.
		http.Error(w, "agentbox: 该账号未绑定 IP 代理", http.StatusBadGateway)
		return config.Proxy{}, false
	}
	if p.Disabled || !config.ValidProxyScheme(p.Scheme) {
		http.Error(w, "agentbox: 账号绑定的 IP 代理已停用或配置无效", http.StatusBadGateway)
		return config.Proxy{}, false
	}
	return p, true
}

// bridgeConnect splices the client socket to an upstream tunnel.
func (s *Server) bridgeConnect(w http.ResponseWriter, r *http.Request, p config.Proxy) {
	target := r.URL.Host
	if target == "" {
		target = r.Host
	}
	up, err := dialThrough(r.Context(), p, target)
	if err != nil {
		log.Printf("proxy bridge: CONNECT %s via %s: %v", target, proxyLabel(p), err)
		http.Error(w, "agentbox proxy bridge: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "agentbox proxy bridge: hijack unsupported", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer down.Close()
	release, tracked := s.track(func() { _ = down.Close(); _ = up.Close() })
	if !tracked {
		return
	}
	defer release()
	if _, err := down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// Whatever the client pipelined behind CONNECT is already buffered; it must
	// go upstream before the raw copy takes over.
	if n := buf.Reader.Buffered(); n > 0 {
		if _, err := io.CopyN(up, buf, int64(n)); err != nil {
			return
		}
	}
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(up, down)
		// Half-close so the upstream sees EOF and finishes its response instead
		// of waiting on a client that is already done sending.
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	_, _ = io.Copy(down, up)
	<-done
}

// bridgeForward relays a plain-http request through the upstream proxy.
func (s *Server) bridgeForward(w http.ResponseWriter, r *http.Request, p config.Proxy) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")

	tr := proxyTransport(p)
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(out)
	if err != nil {
		log.Printf("proxy bridge: %s %s via %s: %v", r.Method, r.URL.Host, proxyLabel(p), err)
		http.Error(w, "agentbox proxy bridge: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// parseProxyAuth splits a Basic Proxy-Authorization header. It reuses the
// stdlib's parser via a throwaway request so the base64/format handling is not
// re-implemented here.
func parseProxyAuth(header string) (user, pass string, ok bool) {
	if header == "" || !strings.HasPrefix(strings.ToLower(header), "basic ") {
		return "", "", false
	}
	r := &http.Request{Header: http.Header{"Authorization": []string{header}}}
	return r.BasicAuth()
}
