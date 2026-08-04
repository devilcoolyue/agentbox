// IP 代理池的管理接口：增删改、连通性探测、批量导入导出。全部 admin 专属，
// 改动经 config.Config 校验后写回 config.json 并即时生效（下一次 exec 注入新的
// 代理环境变量，服务端自己的官方请求下一次调用就换客户端）。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentbox/internal/config"
)

// proxyView is one row of the IP 代理 list. The password never leaves the
// server; the UI only needs to know whether one is set.
type proxyView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	HasPass  bool   `json:"has_pass"`
	Disabled bool   `json:"disabled"`
	URL      string `json:"url"`      // scheme://host:port，不含凭证
	Accounts int    `json:"accounts"` // 绑定该代理的账号数
}

func (s *Server) proxyViewOf(p config.Proxy) proxyView {
	return proxyView{
		ID: p.ID, Name: p.Name, Scheme: p.Scheme, Host: p.Host, Port: p.Port,
		Username: p.Username, HasPass: p.Password != "", Disabled: p.Disabled,
		URL: p.DisplayURL(), Accounts: len(s.cfg.ProxyInUse(p.ID)),
	}
}

// proxyListView is the list plus the bridge status the section header shows.
type proxyListView struct {
	Proxies     []proxyView `json:"proxies"`
	BridgeBind  string      `json:"bridge_bind"`
	BridgeHost  string      `json:"bridge_host"`
	BridgeUp    bool        `json:"bridge_up"`
	BridgeError string      `json:"bridge_error,omitempty"`
}

func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.proxyList(""))
}

func (s *Server) proxyList(bridgeErr string) proxyListView {
	pb := s.cfg.GetProxyBridge()
	out := proxyListView{
		Proxies:     []proxyView{},
		BridgeBind:  pb.Bind,
		BridgeHost:  net.JoinHostPort(pb.Host, portOf(pb.Bind)),
		BridgeUp:    s.bridgeUp.Load(),
		BridgeError: bridgeErr,
	}
	for _, p := range s.cfg.ProxyList() {
		out.Proxies = append(out.Proxies, s.proxyViewOf(p))
	}
	return out
}

// newProxyID mints a random id. Proxies are created from the UI in bulk and
// have no natural key (two entries may share a name, or even a host), so unlike
// accounts the admin never types one.
func newProxyID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "px" + hex.EncodeToString(b)
}

// proxyBody is the create/patch payload. Pointers on the patch path so an
// omitted field keeps its stored value — notably password, which the UI cannot
// echo back and therefore submits empty when unchanged.
type proxyBody struct {
	Name     *string `json:"name"`
	Scheme   *string `json:"scheme"`
	Host     *string `json:"host"`
	Port     *int    `json:"port"`
	Username *string `json:"username"`
	Password *string `json:"password"`
	Disabled *bool   `json:"disabled"`
}

func (s *Server) handleProxyCreate(w http.ResponseWriter, r *http.Request) {
	var body proxyBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	p := config.Proxy{ID: newProxyID(), Scheme: config.ProxySOCKS5}
	if body.Scheme != nil {
		p.Scheme = strings.ToLower(strings.TrimSpace(*body.Scheme))
	}
	if body.Name != nil {
		p.Name = strings.TrimSpace(*body.Name)
	}
	if body.Host != nil {
		p.Host = strings.TrimSpace(*body.Host)
	}
	if body.Port != nil {
		p.Port = *body.Port
	}
	if body.Username != nil {
		p.Username = strings.TrimSpace(*body.Username)
	}
	if body.Password != nil {
		p.Password = *body.Password
	}
	if body.Disabled != nil {
		p.Disabled = *body.Disabled
	}
	if p.Name == "" {
		p.Name = p.Host
	}
	if err := s.cfg.AddProxy(p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.refreshProxyBridge()
	writeJSON(w, http.StatusCreated, s.proxyViewOf(p))
}

func (s *Server) handleProxyPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.cfg.Proxy(id); !ok {
		writeErr(w, http.StatusNotFound, "proxy not found")
		return
	}
	var body proxyBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	patch := config.ProxyPatch{Port: body.Port, Disabled: body.Disabled}
	if body.Name != nil {
		v := strings.TrimSpace(*body.Name)
		patch.Name = &v
	}
	if body.Scheme != nil {
		v := strings.ToLower(strings.TrimSpace(*body.Scheme))
		patch.Scheme = &v
	}
	if body.Host != nil {
		v := strings.TrimSpace(*body.Host)
		patch.Host = &v
	}
	if body.Username != nil {
		v := strings.TrimSpace(*body.Username)
		patch.Username = &v
	}
	// 密码留空 = 不改。弹窗里显示的是掩码，原样提交回来会把真密码抹成掩码串。
	if body.Password != nil && *body.Password != "" {
		patch.Password = body.Password
	}
	p, err := s.cfg.UpdateProxy(id, patch)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.proxyViewOf(p))
}

func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.cfg.Proxy(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "proxy not found")
		return
	}
	// 删除会连带解绑账号——那些账号会立刻改走直连（真实出口 IP）。这是明显的
	// 行为变化，不能顺手做掉，必须让管理员知道自己在解绑谁。
	if users := s.cfg.ProxyInUse(id); len(users) > 0 && r.URL.Query().Get("force") != "1" {
		writeErr(w, http.StatusConflict,
			"仍有 "+strconv.Itoa(len(users))+" 个账号绑定该代理（"+strings.Join(users, "、")+
				"），删除后这些账号将改走服务器直连 IP")
		return
	}
	if err := s.cfg.RemoveProxy(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("proxy %s (%s) deleted", id, p.DisplayURL())
	s.refreshProxyBridge()
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// refreshProxyBridge starts/stops the bridge after the pool changed. Failures
// are logged only: the pool edit itself succeeded, and the bridge state is
// reported by the next list call.
func (s *Server) refreshProxyBridge() {
	if err := s.applyProxyBridge(); err != nil {
		log.Printf("account proxy bridge: %v", err)
	}
}

// --- 连通性探测 ---

// proxyProbeURLs are the exit-IP echo endpoints, tried in order. Several
// because any single one of them can be blocked or down, and a false "代理不通"
// on a working proxy is worse than a slow probe.
var proxyProbeURLs = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://ipinfo.io/ip",
}

const proxyProbeTimeout = 20 * time.Second

type proxyTestResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	ExitIP    string `json:"exit_ip,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleProxyTest probes a proxy: either a saved one by id, or the values
// currently typed into the edit dialog (so an admin can test before saving).
func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
		proxyBody
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	var p config.Proxy
	if body.ID != "" {
		saved, ok := s.cfg.Proxy(body.ID)
		if !ok {
			writeErr(w, http.StatusNotFound, "proxy not found")
			return
		}
		p = saved
	}
	// 弹窗里改了什么就测什么；没填的字段沿用已保存的值（密码尤其如此）。
	if body.Scheme != nil && *body.Scheme != "" {
		p.Scheme = strings.ToLower(strings.TrimSpace(*body.Scheme))
	}
	if body.Host != nil && *body.Host != "" {
		p.Host = strings.TrimSpace(*body.Host)
	}
	if body.Port != nil && *body.Port != 0 {
		p.Port = *body.Port
	}
	if body.Username != nil {
		p.Username = strings.TrimSpace(*body.Username)
	}
	if body.Password != nil && *body.Password != "" {
		p.Password = *body.Password
	}
	p.Disabled = false // 停用的也允许手动探测，否则修不好也测不了
	if p.Name == "" {
		p.Name = p.Host
	}
	if !config.ValidProxyScheme(p.Scheme) {
		writeErr(w, http.StatusBadRequest, "协议只能是 socks5、http 或 https")
		return
	}
	if p.Host == "" || p.Port < 1 || p.Port > 65535 {
		writeErr(w, http.StatusBadRequest, "主机或端口无效")
		return
	}
	writeJSON(w, http.StatusOK, probeProxy(r.Context(), p))
}

// probeProxy fetches an exit-IP echo through p and reports round-trip latency.
func probeProxy(ctx context.Context, p config.Proxy) proxyTestResult {
	client := &http.Client{Timeout: proxyProbeTimeout, Transport: proxyTransport(p)}
	defer client.CloseIdleConnections()
	var lastErr string
	for _, u := range proxyProbeURLs {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("User-Agent", "curl/8.0") // 有些回显站对空 UA 返回 HTML
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Sprintf("%s → HTTP %d", u, resp.StatusCode)
			continue
		}
		ip := strings.TrimSpace(string(raw))
		if net.ParseIP(ip) == nil {
			lastErr = u + " → 响应不是 IP：" + truncate(ip, 80)
			continue
		}
		return proxyTestResult{
			OK: true, LatencyMS: time.Since(start).Milliseconds(), ExitIP: ip, Endpoint: u,
		}
	}
	return proxyTestResult{Error: lastErr}
}

// --- 批量导入 / 导出 ---

// parseProxyLine accepts the shapes proxy vendors hand out:
//
//	scheme://user:pass@host:port
//	host:port:user:pass
//	host:port
//
// with an optional `#名称` suffix. Scheme defaults to socks5 — that is what the
// pool is overwhelmingly made of, and a vendor list that omits it never means
// http.
func parseProxyLine(line string) (config.Proxy, error) {
	line = strings.TrimSpace(line)
	name := ""
	if i := strings.LastIndexByte(line, '#'); i >= 0 {
		name = strings.TrimSpace(line[i+1:])
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return config.Proxy{}, fmt.Errorf("空行")
	}

	p := config.Proxy{ID: newProxyID(), Scheme: config.ProxySOCKS5, Name: name}
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil {
			return config.Proxy{}, fmt.Errorf("无法解析：%s", line)
		}
		p.Scheme = strings.ToLower(u.Scheme)
		if p.Scheme == "socks5h" {
			p.Scheme = config.ProxySOCKS5 // 我们本来就在代理侧解析域名
		}
		p.Host = u.Hostname()
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			return config.Proxy{}, fmt.Errorf("端口无效：%s", line)
		}
		p.Port = port
		if u.User != nil {
			p.Username = u.User.Username()
			p.Password, _ = u.User.Password()
		}
	} else {
		parts := strings.Split(line, ":")
		if len(parts) != 2 && len(parts) != 4 {
			return config.Proxy{}, fmt.Errorf("格式不认识：%s", line)
		}
		p.Host = strings.TrimSpace(parts[0])
		port, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return config.Proxy{}, fmt.Errorf("端口无效：%s", line)
		}
		p.Port = port
		if len(parts) == 4 {
			p.Username, p.Password = parts[2], parts[3]
		}
	}
	if p.Name == "" {
		p.Name = p.Host
	}
	if p.Host == "" || p.Port < 1 || p.Port > 65535 {
		return config.Proxy{}, fmt.Errorf("主机或端口无效：%s", line)
	}
	if !config.ValidProxyScheme(p.Scheme) {
		return config.Proxy{}, fmt.Errorf("协议 %q 不支持：%s", p.Scheme, line)
	}
	return p, nil
}

func (s *Server) handleProxyImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	var (
		list   []config.Proxy
		errs   []string
		seen   = map[string]bool{}
		hadDup int
	)
	for _, p := range s.cfg.ProxyList() {
		seen[p.Scheme+"|"+p.Endpoint()] = true
	}
	for i, line := range strings.Split(body.Text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p, err := parseProxyLine(line)
		if err != nil {
			if len(errs) < 10 {
				errs = append(errs, "第 "+strconv.Itoa(i+1)+" 行："+err.Error())
			}
			continue
		}
		// 同一个 host:port 重复导入只会让列表变噪音，静默跳过并报个数。
		key := p.Scheme + "|" + p.Endpoint()
		if seen[key] {
			hadDup++
			continue
		}
		seen[key] = true
		list = append(list, p)
	}
	added := 0
	if len(list) > 0 {
		n, err := s.cfg.AddProxies(list)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		added = n
		s.refreshProxyBridge()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added":      added,
		"duplicates": hadDup,
		"errors":     errs,
	})
}

// handleProxyExport returns the pool as importable text. It contains the proxy
// passwords in the clear, which is why it is admin-only and returned in the
// response body rather than as a token-in-URL download link.
func (s *Server) handleProxyExport(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	for _, p := range s.cfg.ProxyList() {
		b.WriteString(p.URL())
		if p.Name != "" && p.Name != p.Host {
			b.WriteString("#" + p.Name)
		}
		b.WriteString("\n")
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": b.String()})
}
