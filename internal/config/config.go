// Package config loads, mutates and persists the agentbox server
// configuration. Since the 系统设置 UI can edit accounts and settings at
// runtime, all access goes through mutex-guarded methods and every mutation
// is validated and written back to the config file atomically.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

var (
	accountIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)
	modelIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Account is one entry of the subscription-account pool. Credentials live in
// CredentialsDir on the host and are copied into a session's home directory
// each time the session starts; Env entries (e.g. ANTHROPIC_BASE_URL) are
// injected into the container environment instead.
type Account struct {
	ID             string            `json:"id"`
	Type           string            `json:"type"` // "claude" | "codex"
	Label          string            `json:"label"`
	CredentialsDir string            `json:"credentials_dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`

	// credentials_dir 在配置文件里的原文（可能是相对路径），写回时保留原样。
	rawCredDir string
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

// ModelOption is one selectable model in the chat composer. The list is
// editable in 系统设置 so new models don't require a rebuild.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Config struct {
	Listen         string                   `json:"listen"`
	AuthToken      string                   `json:"auth_token"`
	DataDir        string                   `json:"data_dir"`
	AgentImage     string                   `json:"agent_image"`
	PermissionMode string                   `json:"permission_mode"`
	MaxUploadMB    int64                    `json:"max_upload_mb"`
	Container      ContainerLimits          `json:"container"`
	Tunnel         TunnelConfig             `json:"tunnel"`
	Accounts       []Account                `json:"accounts"`
	Models         map[string][]ModelOption `json:"models,omitempty"`

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
}

type persistConfig struct {
	Listen         string                   `json:"listen"`
	AuthToken      string                   `json:"auth_token"`
	DataDir        string                   `json:"data_dir,omitempty"`
	AgentImage     string                   `json:"agent_image"`
	PermissionMode string                   `json:"permission_mode"`
	MaxUploadMB    int64                    `json:"max_upload_mb"`
	Container      ContainerLimits          `json:"container"`
	Tunnel         TunnelConfig             `json:"tunnel"`
	Accounts       []persistAccount         `json:"accounts"`
	Models         map[string][]ModelOption `json:"models,omitempty"`
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
		Container:      c.Container,
		Tunnel:         c.Tunnel,
		Accounts:       make([]persistAccount, 0, len(c.Accounts)),
		Models:         c.Models,
	}
	for _, a := range c.Accounts {
		dir := a.rawCredDir
		if dir == "" {
			dir = a.CredentialsDir
		}
		out.Accounts = append(out.Accounts, persistAccount{
			ID: a.ID, Type: a.Type, Label: a.Label, CredentialsDir: dir, Env: a.Env,
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

func (c *Config) GetModels() map[string][]ModelOption {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string][]ModelOption, len(c.Models))
	for k, v := range c.Models {
		out[k] = append([]ModelOption(nil), v...)
	}
	return out
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
		Container:      c.Container,
		Tunnel:         c.Tunnel,
		Accounts:       append([]Account(nil), c.Accounts...),
		Models:         c.Models,
		path:           c.path,
		rawDataDir:     c.rawDataDir,
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
	c.Container = work.Container
	c.Tunnel = work.Tunnel
	c.Accounts = work.Accounts
	c.Models = work.Models
	return nil
}

// SettingsPatch carries a partial settings update; nil fields stay unchanged.
type SettingsPatch struct {
	Listen         *string                  `json:"listen"`
	AgentImage     *string                  `json:"agent_image"`
	PermissionMode *string                  `json:"permission_mode"`
	MaxUploadMB    *int64                   `json:"max_upload_mb"`
	Container      *ContainerLimits         `json:"container"`
	Models         map[string][]ModelOption `json:"models"`
	Tunnel         *TunnelConfig            `json:"tunnel"`
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

func (c *Config) UpdateAccount(id, label string, env map[string]string) (Account, error) {
	var out Account
	err := c.mutate(func(w *Config) error {
		for i := range w.Accounts {
			if w.Accounts[i].ID != id {
				continue
			}
			w.Accounts[i].Label = label
			w.Accounts[i].Env = env
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

// Path returns the absolute config file path (immutable after Load).
func (c *Config) Path() string { return c.path }

// ValidAccountID reports whether id is acceptable for a new account.
func ValidAccountID(id string) bool { return accountIDRe.MatchString(id) }

// ValidEnvKey reports whether k is acceptable as an env variable name.
func ValidEnvKey(k string) bool { return envKeyRe.MatchString(k) }
