package config

import (
	"fmt"
	"net"
)

// defaultUsageTelemetryBind sits on the docker bridge gateway after the tunnel
// SOCKS proxy (1080), the account proxy bridge (1081), the transparent network
// control listener (1082) and the model gateway (1083).
const defaultUsageTelemetryBind = "172.17.0.1:1084"

// UsageTelemetryConfig is where the server receives the per-request timing
// Claude Code reports over OpenTelemetry (reachable from containers), and the
// host advertised to them (defaults to Bind's host). On by default; Disabled
// stops the listener and the env that points CLIs at it. Operator settings:
// moving the listener changes the URL every running CLI holds.
type UsageTelemetryConfig struct {
	Bind     string `json:"bind,omitempty"`
	Host     string `json:"host,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

func (c *Config) validateUsageTelemetry() error {
	t := c.UsageTelemetry
	if t.Bind != "" {
		host, _, err := net.SplitHostPort(t.Bind)
		if err != nil {
			return fmt.Errorf("usage_telemetry.bind %q invalid (want host:port): %w", t.Bind, err)
		}
		if t.Host == "" && (host == "" || net.ParseIP(host).IsUnspecified()) {
			return fmt.Errorf("usage_telemetry.host is required when the bind host is empty or a wildcard")
		}
	}
	if t.Disabled {
		return nil
	}
	bind := t.Bind
	if bind == "" {
		bind = defaultUsageTelemetryBind
	}
	gateway := c.ModelGateway.Bind
	if gateway == "" {
		gateway = defaultModelGatewayBind
	}
	others := map[string]string{"proxy_bridge.bind": c.ProxyBridge.Bind, "model_gateway.bind": gateway}
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
			return fmt.Errorf("usage_telemetry.bind %s collides with %s", bind, field)
		}
	}
	return nil
}

// GetUsageTelemetry returns the telemetry config with its defaults filled in.
func (c *Config) GetUsageTelemetry() UsageTelemetryConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.UsageTelemetry
	if out.Bind == "" {
		out.Bind = defaultUsageTelemetryBind
	}
	if out.Host == "" {
		if host, _, err := net.SplitHostPort(out.Bind); err == nil {
			out.Host = host
		}
	}
	return out
}
