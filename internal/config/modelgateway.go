package config

import (
	"fmt"
	"net"
)

// defaultModelGatewayBind sits on the docker bridge gateway next to the tunnel
// SOCKS proxy (1080), the account proxy bridge (1081) and the transparent
// network control listener (1082).
const defaultModelGatewayBind = "172.17.0.1:1083"

// ModelGatewayConfig is where the model gateway listens (reachable from
// containers) and the host advertised to them (defaults to Bind's host). The
// gateway is infrastructure that extensions with model hooks depend on; it
// only listens while one of them is enabled. Bind and host are operator
// settings: moving the listener changes the URL every routed CLI holds.
//
// DedicatedConnections gives every upstream request its own HTTP/1.1 TCP/TLS
// connection, so an interruption closes that connection instead of only
// resetting one HTTP/2 stream.
type ModelGatewayConfig struct {
	Bind                 string `json:"bind,omitempty"`
	Host                 string `json:"host,omitempty"`
	DedicatedConnections bool   `json:"dedicated_connections,omitempty"`
}

// ModelGatewayPatch is the administrator-editable part of the gateway.
type ModelGatewayPatch struct {
	DedicatedConnections *bool `json:"dedicated_connections"`
}

// migrateModelGateway moves the pre-2026-10-17 extensions.gateway section to
// model_gateway. An explicit model_gateway section wins; the old one is
// dropped either way and disappears from the file on the next save.
func (c *Config) migrateModelGateway() {
	old := c.Extensions.LegacyGateway
	c.Extensions.LegacyGateway = nil
	if old == nil || c.ModelGateway != (ModelGatewayConfig{}) {
		return
	}
	c.ModelGateway = ModelGatewayConfig{Bind: old.Bind, Host: old.Host, DedicatedConnections: old.DedicatedConnections}
}

func (c *Config) validateModelGateway() error {
	g := c.ModelGateway
	if g.Bind != "" {
		host, _, err := net.SplitHostPort(g.Bind)
		if err != nil {
			return fmt.Errorf("model_gateway.bind %q invalid (want host:port): %w", g.Bind, err)
		}
		if g.Host == "" && (host == "" || net.ParseIP(host).IsUnspecified()) {
			return fmt.Errorf("model_gateway.host is required when the bind host is empty or a wildcard")
		}
	}
	bind := g.Bind
	if bind == "" {
		bind = defaultModelGatewayBind
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
			return fmt.Errorf("model_gateway.bind %s collides with %s", bind, field)
		}
	}
	return nil
}

// GetModelGateway returns the gateway config with its defaults filled in.
func (c *Config) GetModelGateway() ModelGatewayConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.ModelGateway
	if out.Bind == "" {
		out.Bind = defaultModelGatewayBind
	}
	if out.Host == "" {
		if host, _, err := net.SplitHostPort(out.Bind); err == nil {
			out.Host = host
		}
	}
	return out
}
