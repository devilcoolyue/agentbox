package linkapp

import (
	"agentbox/internal/netaccess"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"agentbox/internal/tunnel"
)

// ErrAuthRejected marks a handshake the server refused for auth reasons — the
// token was revoked or expired (changing the account password revokes all
// tokens). Retrying the same token is pointless; the caller must re-login or
// re-pair.
var ErrAuthRejected = errors.New("认证被拒绝")

// MapStatus is the server's bind result for one requested port map.
type MapStatus struct {
	Port   string `json:"port"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func tlsConfig(insecure bool) *tls.Config {
	return &tls.Config{InsecureSkipVerify: insecure}
}

func httpClient(insecure bool) *http.Client {
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsConfig(insecure)},
	}
}

// baseURL parses and normalises an agentbox base URL.
func baseURL(server string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("服务器地址无效: %s", server)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// Login exchanges a username and password for a session token.
func Login(server, user, password string, insecure bool) (string, error) {
	base, err := baseURL(server)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"username": user, "password": password})
	resp, err := httpClient(insecure).Post(base.String()+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("响应中没有令牌")
	}
	return out.Token, nil
}

// RevokeToken best-effort asks the server to invalidate a session token
// (POST /api/logout). Called on unpair so a copied config.json's token can't
// outlive the unpair; a failure is non-fatal since the token also ages out via
// the server-side TTL.
func RevokeToken(server, token string, insecure bool) error {
	base, err := baseURL(server)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, base.String()+"/api/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient(insecure).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("logout: %s", resp.Status)
	}
	return nil
}

// wsURL builds the tunnel WebSocket URL for a base URL.
func wsURL(base *url.URL) string {
	scheme := "wss"
	if base.Scheme == "http" {
		scheme = "ws"
	}
	return (&url.URL{Scheme: scheme, Host: base.Host, Path: base.Path + "/api/tunnel"}).String()
}

// dialTunnel performs the WebSocket handshake and wraps the connection in a
// yamux session ready to serve proxied streams.
func dialTunnel(ctx context.Context, cfg Config, maps []tunnel.MapSpec) (*yamux.Session, []MapStatus, error) {
	base, err := baseURL(cfg.Server)
	if err != nil {
		return nil, nil, err
	}
	target := wsURL(base)

	dialer := websocket.Dialer{
		TLSClientConfig:  tlsConfig(cfg.Insecure),
		HandshakeTimeout: 15 * time.Second,
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+cfg.Token)
	if cfg.Transparent {
		rules, err := netaccess.NormalizeRules(cfg.Allow)
		if err != nil {
			return nil, nil, err
		}
		raw, _ := json.Marshal(rules)
		hdr.Set(netaccess.CapabilityHeader, netaccess.Protocol)
		hdr.Set(netaccess.RulesHeader, base64.RawURLEncoding.EncodeToString(raw))
	}
	if h := tunnel.EncodeMapSpecs(maps); h != "" {
		hdr.Set(tunnel.MapsHeader, h)
	}

	conn, resp, err := dialer.DialContext(ctx, target, hdr)
	if err != nil {
		if resp != nil {
			detail := respDetail(resp)
			// 401 means the token is dead — a new one would help. Anything else
			// (403 when the feature is switched off server-side, 502 from a
			// proxy) is not fixed by re-authenticating, so keep it plain.
			if resp.StatusCode == http.StatusUnauthorized {
				return nil, nil, fmt.Errorf("连接 %s: %s%s: %w", target, resp.Status, detail, ErrAuthRejected)
			}
			return nil, nil, fmt.Errorf("连接 %s: %s%s", target, resp.Status, detail)
		}
		return nil, nil, fmt.Errorf("连接 %s: %w", target, err)
	}

	if cfg.Transparent && resp.Header.Get(netaccess.CapabilityHeader) != netaccess.Protocol {
		conn.Close()
		return nil, nil, fmt.Errorf("服务端不支持透明访问，请升级服务端或关闭客户端透明访问")
	}
	ycfg := yamux.DefaultConfig()
	ycfg.KeepAliveInterval = 15 * time.Second
	ycfg.ConnectionWriteTimeout = 15 * time.Second
	sess, err := yamux.Server(tunnel.NewWSConn(conn), ycfg)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return sess, parseMapStatus(resp), nil
}

// respDetail pulls the server's {"error": "..."} reason out of a failed
// handshake response.
func respDetail(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var out struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &out) == nil && out.Error != "" {
		return " (" + out.Error + ")"
	}
	return ""
}

// parseMapStatus decodes the server's per-map bind results header.
func parseMapStatus(resp *http.Response) []MapStatus {
	if resp == nil {
		return nil
	}
	raw := resp.Header.Get(tunnel.MapStatusHeader)
	if raw == "" {
		return nil
	}
	var out []MapStatus
	for _, ent := range strings.Split(raw, ",") {
		port, status, _ := strings.Cut(ent, "=")
		st := MapStatus{Port: port, OK: status == "ok"}
		if !st.OK {
			st.Detail = status
		}
		out = append(out, st)
	}
	return out
}
