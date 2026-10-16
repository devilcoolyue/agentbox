package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
)

// ErrExtensionRevision rejects a write based on a stale extension setting.
var ErrExtensionRevision = errors.New("扩展设置已变化，请刷新后重试")

// defaultExtensionGatewayBind sits on the docker bridge gateway next to the
// tunnel SOCKS proxy (1080), the account proxy bridge (1081) and the
// transparent network control listener (1082).
const defaultExtensionGatewayBind = "172.17.0.1:1083"

var extensionIDRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)

// ExtensionsConfig holds the model gateway address and the administrator's
// per-extension settings. Settings are validated structurally here; the
// extension host validates them against each manifest before they are saved.
type ExtensionsConfig struct {
	Gateway ExtensionGateway            `json:"gateway"`
	Items   map[string]ExtensionSetting `json:"items,omitempty"`
}

// ExtensionGateway is where the model gateway listens (reachable from
// containers) and the host advertised to them (defaults to Bind's host).
type ExtensionGateway struct {
	Bind string `json:"bind,omitempty"`
	Host string `json:"host,omitempty"`
}

type ExtensionSetting struct {
	Enabled       bool              `json:"enabled"`
	Version       string            `json:"version,omitempty"`
	Order         int               `json:"order"`
	Config        json.RawMessage   `json:"config,omitempty"`
	Permissions   []string          `json:"permissions"`
	Bindings      ExtensionBindings `json:"bindings"`
	FailurePolicy string            `json:"failure_policy,omitempty"`
	Revision      int64             `json:"revision"`
}

// ExtensionBindings selects the accounts whose model traffic an extension
// inspects: none, the listed accounts, or every eligible account.
type ExtensionBindings struct {
	Mode     string   `json:"mode"`
	Accounts []string `json:"accounts,omitempty"`
}

func (s ExtensionSetting) Clone() ExtensionSetting {
	s.Config = append(json.RawMessage(nil), s.Config...)
	s.Permissions = append([]string(nil), s.Permissions...)
	s.Bindings.Accounts = append([]string(nil), s.Bindings.Accounts...)
	return s
}

// Binds reports whether the setting applies to the account.
func (s ExtensionSetting) Binds(accountID string) bool {
	switch s.Bindings.Mode {
	case "all":
		return true
	case "accounts":
		return slices.Contains(s.Bindings.Accounts, accountID)
	}
	return false
}

// compactConfigs undoes the indentation json.MarshalIndent adds to raw
// extension configs on save, keeping the in-memory form canonical.
func (e *ExtensionsConfig) compactConfigs() {
	for id, s := range e.Items {
		var buf bytes.Buffer
		if len(s.Config) != 0 && json.Compact(&buf, s.Config) == nil {
			s.Config = append(json.RawMessage(nil), buf.Bytes()...)
			e.Items[id] = s
		}
	}
}

func (e ExtensionsConfig) Clone() ExtensionsConfig {
	out := ExtensionsConfig{Gateway: e.Gateway}
	if e.Items != nil {
		out.Items = make(map[string]ExtensionSetting, len(e.Items))
		for id, s := range e.Items {
			out.Items[id] = s.Clone()
		}
	}
	return out
}

func (c *Config) validateExtensions() error {
	e := c.Extensions
	if e.Gateway.Bind != "" {
		host, _, err := net.SplitHostPort(e.Gateway.Bind)
		if err != nil {
			return fmt.Errorf("extensions.gateway.bind %q invalid (want host:port): %w", e.Gateway.Bind, err)
		}
		if e.Gateway.Host == "" && (host == "" || net.ParseIP(host).IsUnspecified()) {
			return fmt.Errorf("extensions.gateway.host is required when the bind host is empty or a wildcard")
		}
	}
	bind := e.Gateway.Bind
	if bind == "" {
		bind = defaultExtensionGatewayBind
	}
	// The other container-facing listeners share the bridge gateway host.
	others := map[string]string{"proxy_bridge.bind": c.ProxyBridge.Bind}
	if c.Tunnel.Enabled || c.Tunnel.Transparent {
		others["tunnel.proxy_bind"] = c.Tunnel.ProxyBind
		network := c.Tunnel.NetworkBind
		if network == "" {
			host, _, _ := net.SplitHostPort(c.Tunnel.ProxyBind)
			network = net.JoinHostPort(host, "1082")
		}
		others["tunnel.network_bind"] = network
	}
	for field, addr := range others {
		if addr != "" && addr == bind {
			return fmt.Errorf("extensions.gateway.bind %s collides with %s", bind, field)
		}
	}
	accounts := map[string]bool{}
	for _, a := range c.Accounts {
		accounts[a.ID] = true
	}
	for id, s := range e.Items {
		if len(id) > 64 || !extensionIDRe.MatchString(id) {
			return fmt.Errorf("extensions.items: invalid extension id %q", id)
		}
		if s.Order < 0 || s.Order > 1000 {
			return fmt.Errorf("extensions.items.%s.order must be 0..1000", id)
		}
		if s.Revision < 0 || s.Revision == math.MaxInt64 {
			return fmt.Errorf("extensions.items.%s.revision invalid", id)
		}
		if len(s.Config) > 64<<10 || (len(s.Config) != 0 && !json.Valid(s.Config)) {
			return fmt.Errorf("extensions.items.%s.config must be JSON of at most 64 KiB", id)
		}
		if len(s.Permissions) > 16 {
			return fmt.Errorf("extensions.items.%s.permissions too long", id)
		}
		switch s.FailurePolicy {
		case "", "fail_open", "fail_closed":
		default:
			return fmt.Errorf("extensions.items.%s.failure_policy must be fail_open or fail_closed", id)
		}
		switch s.Bindings.Mode {
		case "none", "all":
			if len(s.Bindings.Accounts) != 0 {
				return fmt.Errorf("extensions.items.%s.bindings: accounts only apply to mode accounts", id)
			}
		case "accounts":
			seen := map[string]bool{}
			for _, acct := range s.Bindings.Accounts {
				if !accounts[acct] || seen[acct] {
					return fmt.Errorf("extensions.items.%s.bindings: unknown or duplicate account %q", id, acct)
				}
				seen[acct] = true
			}
		default:
			return fmt.Errorf("extensions.items.%s.bindings.mode must be none, accounts or all", id)
		}
	}
	return nil
}

// GetExtensions returns a deep copy with the gateway defaults filled in.
func (c *Config) GetExtensions() ExtensionsConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.Extensions.Clone()
	if out.Gateway.Bind == "" {
		out.Gateway.Bind = defaultExtensionGatewayBind
	}
	if out.Gateway.Host == "" {
		if host, _, err := net.SplitHostPort(out.Gateway.Bind); err == nil {
			out.Gateway.Host = host
		}
	}
	return out
}

// SetExtension replaces one extension's setting if expectedRevision matches
// the stored one (0 for a setting that does not exist yet). The saved setting
// gets the next revision.
func (c *Config) SetExtension(id string, s ExtensionSetting, expectedRevision int64) (ExtensionSetting, error) {
	var out ExtensionSetting
	err := c.mutate(func(w *Config) error {
		current := w.Extensions.Items[id]
		if current.Revision != expectedRevision {
			return ErrExtensionRevision
		}
		next := s.Clone()
		next.Revision = current.Revision + 1
		items := make(map[string]ExtensionSetting, len(w.Extensions.Items)+1)
		for k, v := range w.Extensions.Items {
			items[k] = v
		}
		items[id] = next
		w.Extensions.Items = items
		out = next.Clone()
		return nil
	})
	return out, err
}

// pruneExtensionAccount drops a deleted account from every binding and bumps
// the affected revisions, so a stale browser cannot write it back.
func (w *Config) pruneExtensionAccount(id string) {
	var items map[string]ExtensionSetting
	for key, s := range w.Extensions.Items {
		if s.Bindings.Mode != "accounts" || !slices.Contains(s.Bindings.Accounts, id) {
			continue
		}
		if items == nil {
			items = make(map[string]ExtensionSetting, len(w.Extensions.Items))
			for k, v := range w.Extensions.Items {
				items[k] = v
			}
		}
		next := s.Clone()
		next.Bindings.Accounts = slices.DeleteFunc(next.Bindings.Accounts, func(a string) bool { return a == id })
		next.Revision++
		items[key] = next
	}
	if items != nil {
		w.Extensions.Items = items
	}
}
