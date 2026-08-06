// Package config loads, mutates and persists the agentbox server
// configuration. Since the 系统设置 UI can edit accounts and settings at
// runtime, all access goes through mutex-guarded methods and every mutation
// is validated and written back to the config file atomically.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

var (
	accountIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)
	proxyIDRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)
	modelIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(\[1m\])?$`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Account is one entry of the subscription-account pool. Credentials live in
// CredentialsDir on the host and are copied into a session's home directory
// each time the session starts; Env entries (e.g. ANTHROPIC_BASE_URL) are
// injected into the container environment instead.
//
// ProxyID binds every request made on this account's behalf — the agent's
// official API calls inside the container as well as the server's own OAuth /
// key-test calls — to one entry of Proxies. Empty means direct.
type Account struct {
	ID             string            `json:"id"`
	Type           string            `json:"type"` // "claude" | "codex"
	Label          string            `json:"label"`
	CredentialsDir string            `json:"credentials_dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	ProxyID        string            `json:"proxy_id,omitempty"`

	// credentials_dir 在配置文件里的原文（可能是相对路径），写回时保留原样。
	rawCredDir string
}

// Proxy is one outbound IP proxy in the pool. Accounts reference it by ID.
//
// Disabled is a kill switch, not a "route around it" flag: an account bound to
// a disabled proxy fails its requests rather than silently falling back to the
// server's own IP. Leaking the real egress IP is exactly what binding a proxy
// was meant to prevent, so this path fails closed on purpose.
type Proxy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Scheme   string `json:"scheme"` // socks5 | http | https
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

const (
	ProxySOCKS5 = "socks5"
	ProxyHTTP   = "http"
	ProxyHTTPS  = "https"
)

// Endpoint is the proxy's dial address.
func (p Proxy) Endpoint() string { return net.JoinHostPort(p.Host, strconv.Itoa(p.Port)) }

// URL renders the proxy as a scheme://[user:pass@]host:port string — what the
// admin UI shows and what Import parses back.
func (p Proxy) URL() string {
	auth := ""
	if p.Username != "" {
		auth = url.QueryEscape(p.Username)
		if p.Password != "" {
			auth += ":" + url.QueryEscape(p.Password)
		}
		auth += "@"
	}
	return p.Scheme + "://" + auth + p.Endpoint()
}

// DisplayURL is URL without the credentials — safe for logs, error messages and
// any listing a non-admin might see.
func (p Proxy) DisplayURL() string { return p.Scheme + "://" + p.Endpoint() }

// ValidProxyScheme reports whether s is a scheme the dialer knows.
func ValidProxyScheme(s string) bool {
	switch s {
	case ProxySOCKS5, ProxyHTTP, ProxyHTTPS:
		return true
	}
	return false
}

type ContainerLimits struct {
	MemoryMB  int64   `json:"memory_mb"`
	CPUs      float64 `json:"cpus"`
	PidsLimit int64   `json:"pids_limit"`
	Network   string  `json:"network"`
}

// TunnelConfig controls the reverse-tunnel feature: a local "abox-link" client
// dials in over WebSocket and the server exposes a shared SOCKS5 proxy on the
// docker bridge gateway so containers can reach the user's LAN/intranet through
// their own machine. ProxyBind is where the server listens (must be reachable
// from containers, e.g. the bridge gateway); ProxyHost is what gets advertised
// to containers in the injected proxy URL (defaults to ProxyBind's host).
type TunnelConfig struct {
	Enabled   bool   `json:"enabled"`
	ProxyBind string `json:"proxy_bind,omitempty"` // host:port the SOCKS5 proxy binds, default defaultTunnelBind
	ProxyHost string `json:"proxy_host,omitempty"` // host containers use to reach it, default = host of ProxyBind
}

// defaultTunnelBind is the docker bridge gateway — the one host address every
// container on the default bridge can reach.
const defaultTunnelBind = "172.17.0.1:1080"

// ProxyBridgeConfig controls the local HTTP CONNECT proxy that fronts the
// account proxy pool. Containers are handed HTTP_PROXY/HTTPS_PROXY pointing at
// this bridge and the bridge forwards to the upstream proxy bound to the
// calling account.
//
// The indirection exists because the pool is mostly SOCKS5 while the agent CLIs
// are not: Claude Code is Node/undici, whose proxy support is HTTP(S)-only, so
// a socks5:// URL in HTTPS_PROXY is simply ignored. Speaking plain HTTP proxy to
// the container and doing the SOCKS5 leg ourselves makes every upstream scheme
// work for both CLIs.
//
// Bind is where the bridge listens (must be reachable from containers, so the
// docker bridge gateway by default); Host is what gets advertised in the
// injected URL, defaulting to Bind's host.
type ProxyBridgeConfig struct {
	Bind string `json:"bind,omitempty"`
	Host string `json:"host,omitempty"`
}

// defaultProxyBridgeBind sits on the same gateway as the tunnel proxy, one port
// over.
const defaultProxyBridgeBind = "172.17.0.1:1081"

// ModelOption is one selectable model in the chat composer. The list is
// editable in 系统设置 so new models don't require a rebuild.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// TokenRates is one tier of per-million-token prices in USD. The four buckets
// match store.UsageEvent's disjoint ones: Input counts input that missed the
// cache, CacheRead the part served from cache, CacheWrite the part written into
// it. A rate left at 0 prices that bucket free, which is what the "-" cells in
// OpenAI's table mean.
type TokenRates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// ModelPrice is the rate sheet for one model, used to price usage the provider
// itself doesn't price — Codex reports tokens only. Claude reports its own cost
// and is never priced from this table.
//
// The embedded TokenRates is the short-context tier (flat keys in JSON). Models
// that charge more once a prompt gets big carry a second tier: prompts over
// LongContextOver tokens are billed entirely at Long's rates — it is a cliff,
// not a surcharge on the excess.
//
// Keys of Config.Pricing are looked up in this order: the exact model id, then
// the agent name ("codex") as the catch-all for turns whose event never names a
// model. No match means no price, and the row records at cost 0.
type ModelPrice struct {
	TokenRates
	LongContextOver int64       `json:"long_context_over,omitempty"`
	Long            *TokenRates `json:"long,omitempty"`
}

// Rates picks the tier that applies to a turn whose prompt was promptTokens
// long (everything fed in: cache misses plus cache hits).
func (p ModelPrice) Rates(promptTokens int64) TokenRates {
	if p.Long != nil && p.LongContextOver > 0 && promptTokens > p.LongContextOver {
		return *p.Long
	}
	return p.TokenRates
}

// TerminalTips drives the rotating hint ticker in the terminal page header.
// Editable in 系统设置 and pushed to every user (admin and regular) via /me so
// it shows for all. Tips rotate one at a time; IntervalSec <= 0 disables
// rotation (only the first tip shows). Animation names the transition style —
// currently only "scroll" (a vertical roll); kept as a field so more styles can
// be added without a schema change.
type TerminalTips struct {
	Tips        []string `json:"tips"`
	IntervalSec int      `json:"interval_sec"`
	Animation   string   `json:"animation"`
}

const (
	defaultTipInterval = 4   // 秒：新配置未显式设置时的默认轮播频率
	maxTips            = 30  // 提示语条数上限
	maxTipLen          = 200 // 单条提示语字符（rune）上限
)

// defaultTerminalTips is seeded when a config has no terminal_tips block yet, so
// existing installs keep the original single hint.
func defaultTerminalTips() TerminalTips {
	return TerminalTips{
		Tips:        []string{"可直接粘贴图片，路径可点击预览"},
		IntervalSec: defaultTipInterval,
		Animation:   "scroll",
	}
}

// sanitizeTips trims/drops blank lines and caps count and length, so the admin
// textarea can be pasted in freely without tripping validation.
func sanitizeTips(tt TerminalTips) TerminalTips {
	clean := make([]string, 0, len(tt.Tips))
	for _, t := range tt.Tips {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > maxTipLen {
			t = string([]rune(t)[:maxTipLen])
		}
		clean = append(clean, t)
		if len(clean) >= maxTips {
			break
		}
	}
	tt.Tips = clean
	if tt.Animation == "" {
		tt.Animation = "scroll"
	}
	return tt
}

type Config struct {
	Listen         string                   `json:"listen"`
	AuthToken      string                   `json:"auth_token"`
	DataDir        string                   `json:"data_dir"`
	AgentImage     string                   `json:"agent_image"`
	PermissionMode string                   `json:"permission_mode"`
	MaxUploadMB    int64                    `json:"max_upload_mb"`
	IdleTimeoutMin int64                    `json:"idle_timeout_min"` // 会话空闲自动停机的分钟数；0 表示关闭
	Container      ContainerLimits          `json:"container"`
	Tunnel         TunnelConfig             `json:"tunnel"`
	ProxyBridge    ProxyBridgeConfig        `json:"proxy_bridge"`
	Accounts       []Account                `json:"accounts"`
	Proxies        []Proxy                  `json:"proxies,omitempty"`
	Models         map[string][]ModelOption `json:"models,omitempty"`
	TerminalTips   TerminalTips             `json:"terminal_tips"`
	Pricing        map[string]ModelPrice    `json:"pricing,omitempty"`

	mu         sync.RWMutex
	path       string
	rawDataDir string // data_dir 原文，写回时保留
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Listen:         "127.0.0.1:8080",
		AgentImage:     "agentbox-agent:latest",
		PermissionMode: "bypassPermissions",
		MaxUploadMB:    512,
		IdleTimeoutMin: 30, // 缺省 30 分钟空闲即停机；config 里显式写 0 可关闭
		Container: ContainerLimits{
			MemoryMB:  2048,
			CPUs:      2,
			PidsLimit: 512,
			Network:   "bridge",
		},
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	if cfg.Models == nil {
		cfg.Models = map[string][]ModelOption{}
	}
	if len(cfg.Models[AgentClaude]) == 0 {
		cfg.Models[AgentClaude] = []ModelOption{
			{ID: "claude-fable-5", Label: "Fable 5"},
			{ID: "claude-opus-4-8", Label: "Opus 4.8"},
			{ID: "claude-sonnet-5", Label: "Sonnet 5"},
			{ID: "claude-haiku-4-5", Label: "Haiku 4.5"},
		}
	}
	if len(cfg.Models[AgentCodex]) == 0 {
		cfg.Models[AgentCodex] = []ModelOption{
			{ID: "gpt-5.5", Label: "GPT-5.5"},
			{ID: "gpt-5.5-codex", Label: "GPT-5.5 Codex"},
		}
	}

	// 未配置 terminal_tips（老配置文件里没这一块）时，补上原来的单条提示，
	// 避免终端页顶栏空掉。已显式配置的（哪怕 interval 为 0）尊重原样。
	if len(cfg.TerminalTips.Tips) == 0 {
		cfg.TerminalTips = defaultTerminalTips()
	} else {
		cfg.TerminalTips = sanitizeTips(cfg.TerminalTips)
	}

	base := filepath.Dir(cfg.path)
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}

	cfg.rawDataDir = cfg.DataDir
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	cfg.DataDir = resolve(cfg.DataDir)

	if cfg.Tunnel.Enabled && cfg.Tunnel.ProxyBind == "" {
		cfg.Tunnel.ProxyBind = defaultTunnelBind
	}
	if cfg.ProxyBridge.Bind == "" {
		cfg.ProxyBridge.Bind = defaultProxyBridgeBind
	}

	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		a.rawCredDir = a.CredentialsDir
		a.CredentialsDir = resolve(a.CredentialsDir)
	}
	if err := cfg.validateLocked(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateLocked checks the whole config; callers must hold at least a read
// lock (Load runs before the config is shared, which also counts).
func (c *Config) validateLocked() error {
	if len(c.AuthToken) < 8 || c.AuthToken == "CHANGE_ME_TO_A_LONG_RANDOM_TOKEN" {
		return fmt.Errorf("auth_token must be a secret of at least 8 characters")
	}
	if c.Listen == "" {
		return fmt.Errorf("listen must not be empty")
	}
	if c.AgentImage == "" {
		return fmt.Errorf("agent_image must not be empty")
	}
	if c.MaxUploadMB < 1 {
		return fmt.Errorf("max_upload_mb must be >= 1")
	}
	if c.IdleTimeoutMin < 0 {
		return fmt.Errorf("idle_timeout_min must be >= 0 (0 关闭自动停机)")
	}
	switch c.PermissionMode {
	case "default", "acceptEdits", "plan", "bypassPermissions":
	default:
		return fmt.Errorf("permission_mode %q invalid (default|acceptEdits|plan|bypassPermissions)", c.PermissionMode)
	}
	if c.Container.MemoryMB < 128 {
		return fmt.Errorf("container.memory_mb must be >= 128")
	}
	if c.Container.CPUs <= 0 {
		return fmt.Errorf("container.cpus must be > 0")
	}
	if c.Container.PidsLimit < 16 {
		return fmt.Errorf("container.pids_limit must be >= 16")
	}
	if c.Container.Network == "" {
		return fmt.Errorf("container.network must not be empty")
	}
	if c.Tunnel.Enabled {
		host, _, err := net.SplitHostPort(c.Tunnel.ProxyBind)
		if err != nil {
			return fmt.Errorf("tunnel.proxy_bind %q invalid (want host:port): %w", c.Tunnel.ProxyBind, err)
		}
		// When binding to a wildcard/empty host, the advertised host can't be
		// derived from the bind address, so it must be given explicitly —
		// otherwise the injected proxy URL would point at 0.0.0.0.
		if c.Tunnel.ProxyHost == "" && (host == "" || net.ParseIP(host).IsUnspecified()) {
			return fmt.Errorf("tunnel.proxy_host is required when proxy_bind host is empty or a wildcard (%q)", c.Tunnel.ProxyBind)
		}
	}
	if _, _, err := net.SplitHostPort(c.ProxyBridge.Bind); err != nil {
		return fmt.Errorf("proxy_bridge.bind %q invalid (want host:port): %w", c.ProxyBridge.Bind, err)
	}
	proxySeen := map[string]bool{}
	for i := range c.Proxies {
		p := &c.Proxies[i]
		if !proxyIDRe.MatchString(p.ID) {
			return fmt.Errorf("proxy id %q invalid (小写字母数字开头，可含 - _，2-32 位)", p.ID)
		}
		if proxySeen[p.ID] {
			return fmt.Errorf("duplicate proxy id %q", p.ID)
		}
		proxySeen[p.ID] = true
		if !ValidProxyScheme(p.Scheme) {
			return fmt.Errorf("proxy %q: scheme must be socks5, http or https", p.ID)
		}
		if err := validProxyHost(p.Host); err != nil {
			return fmt.Errorf("proxy %q: %w", p.ID, err)
		}
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("proxy %q: port must be between 1 and 65535", p.ID)
		}
		// The credentials end up in a proxy URL and a Proxy-Authorization
		// header; control characters there would let one field bleed into the
		// next.
		if strings.ContainsAny(p.Username, " \t\r\n:@") || strings.ContainsAny(p.Password, " \t\r\n@") {
			return fmt.Errorf("proxy %q: 用户名/密码含非法字符", p.ID)
		}
		if p.Name == "" {
			p.Name = p.Host
		}
	}
	seen := map[string]bool{}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if !accountIDRe.MatchString(a.ID) {
			return fmt.Errorf("account id %q invalid (小写字母数字开头，可含 - _，2-32 位)", a.ID)
		}
		if seen[a.ID] {
			return fmt.Errorf("duplicate account id %q", a.ID)
		}
		seen[a.ID] = true
		if a.Type != AgentClaude && a.Type != AgentCodex {
			return fmt.Errorf("account %q: type must be claude or codex", a.ID)
		}
		if a.Label == "" {
			a.Label = a.ID
		}
		if a.ProxyID != "" && !proxySeen[a.ProxyID] {
			return fmt.Errorf("account %q: proxy_id %q 不存在", a.ID, a.ProxyID)
		}
		for k := range a.Env {
			if !envKeyRe.MatchString(k) {
				return fmt.Errorf("account %q: env key %q invalid", a.ID, k)
			}
		}
	}
	for agent, opts := range c.Models {
		if agent != AgentClaude && agent != AgentCodex {
			return fmt.Errorf("models: unknown agent type %q", agent)
		}
		for _, o := range opts {
			if o.Label == "" || !modelIDRe.MatchString(o.ID) {
				return fmt.Errorf("models.%s: entry %q/%q invalid", agent, o.Label, o.ID)
			}
		}
	}
	if c.TerminalTips.IntervalSec < 0 || c.TerminalTips.IntervalSec > 3600 {
		return fmt.Errorf("terminal_tips.interval_sec must be between 0 and 3600 (0 关闭轮播)")
	}
	switch c.TerminalTips.Animation {
	case "", "scroll":
	default:
		return fmt.Errorf("terminal_tips.animation %q invalid (scroll)", c.TerminalTips.Animation)
	}
	return nil
}

// --- 持久化 ---

// persist* mirror the JSON schema of the config file so writes keep the
// original key order and the original (possibly relative) path spellings.
type persistAccount struct {
	ID             string            `json:"id"`
	Type           string            `json:"type"`
	Label          string            `json:"label"`
	CredentialsDir string            `json:"credentials_dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	ProxyID        string            `json:"proxy_id,omitempty"`
}

type persistConfig struct {
	Listen         string                   `json:"listen"`
	AuthToken      string                   `json:"auth_token"`
	DataDir        string                   `json:"data_dir,omitempty"`
	AgentImage     string                   `json:"agent_image"`
	PermissionMode string                   `json:"permission_mode"`
	MaxUploadMB    int64                    `json:"max_upload_mb"`
	IdleTimeoutMin int64                    `json:"idle_timeout_min"`
	Container      ContainerLimits          `json:"container"`
	Tunnel         TunnelConfig             `json:"tunnel"`
	ProxyBridge    ProxyBridgeConfig        `json:"proxy_bridge"`
	Accounts       []persistAccount         `json:"accounts"`
	Proxies        []Proxy                  `json:"proxies,omitempty"`
	Models         map[string][]ModelOption `json:"models,omitempty"`
	TerminalTips   TerminalTips             `json:"terminal_tips"`
	Pricing        map[string]ModelPrice    `json:"pricing,omitempty"`
}

// saveLocked writes the config file atomically; callers must hold the write
// lock. 0600 because the file carries the auth token and account env secrets.
func (c *Config) saveLocked() error {
	out := persistConfig{
		Listen:         c.Listen,
		AuthToken:      c.AuthToken,
		DataDir:        c.rawDataDir,
		AgentImage:     c.AgentImage,
		PermissionMode: c.PermissionMode,
		MaxUploadMB:    c.MaxUploadMB,
		IdleTimeoutMin: c.IdleTimeoutMin,
		Container:      c.Container,
		Tunnel:         c.Tunnel,
		ProxyBridge:    c.ProxyBridge,
		Accounts:       make([]persistAccount, 0, len(c.Accounts)),
		Proxies:        c.Proxies,
		Models:         c.Models,
		TerminalTips:   c.TerminalTips,
		Pricing:        c.Pricing,
	}
	for _, a := range c.Accounts {
		dir := a.rawCredDir
		if dir == "" {
			dir = a.CredentialsDir
		}
		out.Accounts = append(out.Accounts, persistAccount{
			ID: a.ID, Type: a.Type, Label: a.Label, CredentialsDir: dir,
			Env: a.Env, ProxyID: a.ProxyID,
		})
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// --- 读访问 ---

func (c *Config) Account(id string) (Account, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, a := range c.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

func (c *Config) AccountList() []Account {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Account, len(c.Accounts))
	copy(out, c.Accounts)
	return out
}

func (c *Config) GetAuthToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AuthToken
}

func (c *Config) GetListen() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Listen
}

func (c *Config) GetAgentImage() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentImage
}

func (c *Config) GetPermissionMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PermissionMode
}

func (c *Config) GetMaxUploadMB() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MaxUploadMB
}

// GetIdleTimeoutMin returns the idle-stop threshold in minutes; 0 disables the
// idle reaper.
func (c *Config) GetIdleTimeoutMin() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IdleTimeoutMin
}

func (c *Config) GetContainer() ContainerLimits {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Container
}

// GetTunnel returns the reverse-tunnel config. ProxyHost is filled in from
// ProxyBind's host when left blank, so callers get a ready-to-use advertise
// address.
func (c *Config) GetTunnel() TunnelConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t := c.Tunnel
	if t.ProxyHost == "" && t.ProxyBind != "" {
		if host, _, err := net.SplitHostPort(t.ProxyBind); err == nil {
			t.ProxyHost = host
		}
	}
	return t
}

// GetProxyBridge returns the local proxy-bridge config with Host filled in from
// Bind when left blank, so callers get a ready-to-advertise address.
func (c *Config) GetProxyBridge() ProxyBridgeConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	pb := c.ProxyBridge
	if pb.Bind == "" {
		pb.Bind = defaultProxyBridgeBind
	}
	if pb.Host == "" {
		if host, _, err := net.SplitHostPort(pb.Bind); err == nil {
			pb.Host = host
		}
	}
	return pb
}

func (c *Config) ProxyList() []Proxy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Proxy, len(c.Proxies))
	copy(out, c.Proxies)
	return out
}

func (c *Config) Proxy(id string) (Proxy, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, p := range c.Proxies {
		if p.ID == id {
			return p, true
		}
	}
	return Proxy{}, false
}

// AccountProxy resolves the proxy bound to an account. The bool distinguishes
// "no proxy bound" from "bound to something": callers must not treat a dangling
// or disabled binding as direct egress.
func (c *Config) AccountProxy(acctID string) (Proxy, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, a := range c.Accounts {
		if a.ID != acctID {
			continue
		}
		if a.ProxyID == "" {
			return Proxy{}, false
		}
		for _, p := range c.Proxies {
			if p.ID == a.ProxyID {
				return p, true
			}
		}
		// Referential integrity is enforced on write, so this only happens if
		// config.json was hand-edited. Report it as "bound but broken".
		return Proxy{ID: a.ProxyID, Disabled: true}, true
	}
	return Proxy{}, false
}

// ProxyInUse lists the ids of accounts bound to a proxy.
func (c *Config) ProxyInUse(proxyID string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for _, a := range c.Accounts {
		if a.ProxyID == proxyID {
			out = append(out, a.ID)
		}
	}
	return out
}

func (c *Config) GetModels() map[string][]ModelOption {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string][]ModelOption, len(c.Models))
	for k, v := range c.Models {
		out[k] = append([]ModelOption(nil), v...)
	}
	return out
}

// GetTerminalTips returns a copy of the terminal-page hint ticker config; the
// slice is copied so callers can't mutate the shared config.
func (c *Config) GetTerminalTips() TerminalTips {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tt := c.TerminalTips
	tt.Tips = append([]string(nil), c.TerminalTips.Tips...)
	return tt
}

// Price resolves the rate sheet for one usage row: exact model id first, then
// the agent name as the catch-all. The bool is false when nothing matches, so
// callers can tell "no price configured" from "configured as free".
func (c *Config) Price(agent, model string) (ModelPrice, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p, ok := c.Pricing[model]; ok && model != "" {
		return p, true
	}
	if p, ok := c.Pricing[agent]; ok && agent != "" {
		return p, true
	}
	return ModelPrice{}, false
}

// --- 写访问：全部先在副本上验证，通过后才落盘并生效 ---

// mutate runs fn on a shallow working copy of the mutable fields, validates
// and persists the result, and only then swaps it in.
func (c *Config) mutate(fn func(*Config) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	work := &Config{
		Listen:         c.Listen,
		AuthToken:      c.AuthToken,
		DataDir:        c.DataDir,
		AgentImage:     c.AgentImage,
		PermissionMode: c.PermissionMode,
		MaxUploadMB:    c.MaxUploadMB,
		IdleTimeoutMin: c.IdleTimeoutMin,
		Container:      c.Container,
		Tunnel:         c.Tunnel,
		ProxyBridge:    c.ProxyBridge,
		Accounts:       append([]Account(nil), c.Accounts...),
		Proxies:        append([]Proxy(nil), c.Proxies...),
		Models:         c.Models,
		TerminalTips:   c.TerminalTips,
		// 这份工作副本会被整体写回 config.json：**漏抄一个字段就等于从文件里
		// 删掉它**。加字段时必须同时加到这里。
		Pricing:    c.Pricing,
		path:       c.path,
		rawDataDir: c.rawDataDir,
	}
	if err := fn(work); err != nil {
		return err
	}
	if err := work.validateLocked(); err != nil {
		return err
	}
	if err := work.saveLocked(); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	c.Listen = work.Listen
	c.AuthToken = work.AuthToken
	c.AgentImage = work.AgentImage
	c.PermissionMode = work.PermissionMode
	c.MaxUploadMB = work.MaxUploadMB
	c.IdleTimeoutMin = work.IdleTimeoutMin
	c.Container = work.Container
	c.Tunnel = work.Tunnel
	c.ProxyBridge = work.ProxyBridge
	c.Accounts = work.Accounts
	c.Proxies = work.Proxies
	c.Models = work.Models
	c.TerminalTips = work.TerminalTips
	c.Pricing = work.Pricing
	return nil
}

// SettingsPatch carries a partial settings update; nil fields stay unchanged.
type SettingsPatch struct {
	Listen         *string                  `json:"listen"`
	AgentImage     *string                  `json:"agent_image"`
	PermissionMode *string                  `json:"permission_mode"`
	MaxUploadMB    *int64                   `json:"max_upload_mb"`
	IdleTimeoutMin *int64                   `json:"idle_timeout_min"`
	Container      *ContainerLimits         `json:"container"`
	Models         map[string][]ModelOption `json:"models"`
	Tunnel         *TunnelConfig            `json:"tunnel"`
	ProxyBridge    *ProxyBridgeConfig       `json:"proxy_bridge"`
	TerminalTips   *TerminalTips            `json:"terminal_tips"`
}

func (c *Config) ApplySettings(p SettingsPatch) error {
	return c.mutate(func(w *Config) error {
		if p.Listen != nil {
			w.Listen = *p.Listen
		}
		if p.AgentImage != nil {
			w.AgentImage = *p.AgentImage
		}
		if p.PermissionMode != nil {
			w.PermissionMode = *p.PermissionMode
		}
		if p.MaxUploadMB != nil {
			w.MaxUploadMB = *p.MaxUploadMB
		}
		if p.IdleTimeoutMin != nil {
			w.IdleTimeoutMin = *p.IdleTimeoutMin
		}
		if p.Container != nil {
			w.Container = *p.Container
		}
		if p.Models != nil {
			w.Models = p.Models
		}
		if p.Tunnel != nil {
			w.Tunnel = *p.Tunnel
			if w.Tunnel.Enabled && w.Tunnel.ProxyBind == "" {
				w.Tunnel.ProxyBind = defaultTunnelBind // same default as Load
			}
		}
		if p.ProxyBridge != nil {
			w.ProxyBridge = *p.ProxyBridge
			if w.ProxyBridge.Bind == "" {
				w.ProxyBridge.Bind = defaultProxyBridgeBind // same default as Load
			}
		}
		if p.TerminalTips != nil {
			w.TerminalTips = sanitizeTips(*p.TerminalTips)
		}
		return nil
	})
}

func (c *Config) SetAuthToken(tok string) error {
	return c.mutate(func(w *Config) error {
		w.AuthToken = tok
		return nil
	})
}

func (c *Config) AddAccount(a Account) error {
	return c.mutate(func(w *Config) error {
		for _, x := range w.Accounts {
			if x.ID == a.ID {
				return fmt.Errorf("账号 ID %q 已存在", a.ID)
			}
		}
		w.Accounts = append(w.Accounts, a)
		return nil
	})
}

// AccountPatch is a partial account update; nil fields stay unchanged. It is a
// struct rather than positional arguments because callers touch disjoint parts
// (the relay helpers only ever rewrite Env, the edit dialog only Label/ProxyID)
// and passing the current value back in for the rest invites clobbering.
type AccountPatch struct {
	Label   *string
	Env     *map[string]string
	ProxyID *string
}

func (c *Config) UpdateAccount(id string, p AccountPatch) (Account, error) {
	var out Account
	err := c.mutate(func(w *Config) error {
		for i := range w.Accounts {
			if w.Accounts[i].ID != id {
				continue
			}
			if p.Label != nil {
				w.Accounts[i].Label = *p.Label
			}
			if p.Env != nil {
				w.Accounts[i].Env = *p.Env
			}
			if p.ProxyID != nil {
				w.Accounts[i].ProxyID = *p.ProxyID
			}
			out = w.Accounts[i]
			return nil
		}
		return fmt.Errorf("account %q not found", id)
	})
	return out, err
}

func (c *Config) RemoveAccount(id string) error {
	return c.mutate(func(w *Config) error {
		for i := range w.Accounts {
			if w.Accounts[i].ID == id {
				w.Accounts = append(w.Accounts[:i:i], w.Accounts[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("account %q not found", id)
	})
}

// --- 代理池写访问 ---

func (c *Config) AddProxy(p Proxy) error {
	return c.mutate(func(w *Config) error {
		for _, x := range w.Proxies {
			if x.ID == p.ID {
				return fmt.Errorf("代理 ID %q 已存在", p.ID)
			}
		}
		w.Proxies = append(w.Proxies, p)
		return nil
	})
}

// ProxyPatch is a partial proxy update; nil fields stay unchanged.
type ProxyPatch struct {
	Name     *string
	Scheme   *string
	Host     *string
	Port     *int
	Username *string
	Password *string
	Disabled *bool
}

func (c *Config) UpdateProxy(id string, p ProxyPatch) (Proxy, error) {
	var out Proxy
	err := c.mutate(func(w *Config) error {
		for i := range w.Proxies {
			if w.Proxies[i].ID != id {
				continue
			}
			x := &w.Proxies[i]
			if p.Name != nil {
				x.Name = *p.Name
			}
			if p.Scheme != nil {
				x.Scheme = *p.Scheme
			}
			if p.Host != nil {
				x.Host = *p.Host
			}
			if p.Port != nil {
				x.Port = *p.Port
			}
			if p.Username != nil {
				x.Username = *p.Username
			}
			if p.Password != nil {
				x.Password = *p.Password
			}
			if p.Disabled != nil {
				x.Disabled = *p.Disabled
			}
			out = *x
			return nil
		}
		return fmt.Errorf("proxy %q not found", id)
	})
	return out, err
}

// RemoveProxy deletes a proxy. Accounts still bound to it would fail validation
// (which would abort the whole write), so the binding is cleared in the same
// mutation — one atomic config write, no half-applied state.
func (c *Config) RemoveProxy(id string) error {
	return c.mutate(func(w *Config) error {
		idx := -1
		for i := range w.Proxies {
			if w.Proxies[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("proxy %q not found", id)
		}
		w.Proxies = append(w.Proxies[:idx:idx], w.Proxies[idx+1:]...)
		for i := range w.Accounts {
			if w.Accounts[i].ProxyID == id {
				w.Accounts[i].ProxyID = ""
			}
		}
		return nil
	})
}

// AddProxies appends a batch (bulk import) in one write; ids already present
// are skipped and reported back as the second return value.
func (c *Config) AddProxies(list []Proxy) (added int, err error) {
	err = c.mutate(func(w *Config) error {
		have := map[string]bool{}
		for _, x := range w.Proxies {
			have[x.ID] = true
		}
		added = 0
		for _, p := range list {
			if have[p.ID] {
				continue
			}
			have[p.ID] = true
			w.Proxies = append(w.Proxies, p)
			added++
		}
		return nil
	})
	return added, err
}

// Path returns the absolute config file path (immutable after Load).
func (c *Config) Path() string { return c.path }

// validProxyHost rejects anything that would not survive being pasted into a
// proxy URL or a CONNECT request line.
func validProxyHost(h string) error {
	if h == "" {
		return fmt.Errorf("host must not be empty")
	}
	if len(h) > 255 || strings.ContainsAny(h, " \t\r\n/@?#\\\"") {
		return fmt.Errorf("host %q invalid", h)
	}
	return nil
}

// ValidProxyID reports whether id is acceptable for a new proxy.
func ValidProxyID(id string) bool { return proxyIDRe.MatchString(id) }

// ValidAccountID reports whether id is acceptable for a new account.
func ValidAccountID(id string) bool { return accountIDRe.MatchString(id) }

// ValidEnvKey reports whether k is acceptable as an env variable name.
func ValidEnvKey(k string) bool { return envKeyRe.MatchString(k) }
