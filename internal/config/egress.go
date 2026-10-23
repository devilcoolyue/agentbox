package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Workspace egress modes (container.egress).
//
// EgressOpen is the historical behaviour: a bridge workspace can reach anything
// the host can route to, and the account proxy is only advisory (HTTP(S)_PROXY
// env that any tool may ignore).
//
// EgressRestricted locks each workspace network namespace down to agentbox's
// own container-facing listeners on the host (account proxy bridge, tunnel,
// network control, model gateway, usage telemetry and the dynamic bridge
// ports). Direct internet access, other host services (sshd, databases…) and
// other workspaces become unreachable, so the only way out is the account's
// bound egress proxy. Accounts without a bound proxy get no internet at all.
const (
	EgressOpen       = "open"
	EgressRestricted = "restricted"
)

// defaultDynamicPorts is where per-request bridge listeners (Git transports,
// Git terminal credentials, delegation MCP) bind. A fixed range lets the
// restricted egress rules allow them without opening every host port.
const defaultDynamicPorts = "30000-30999"

// EgressMode normalizes container.egress; empty means open.
func (l ContainerLimits) EgressMode() string {
	if l.Egress == EgressRestricted {
		return EgressRestricted
	}
	return EgressOpen
}

// PortRange is an inclusive TCP port range.
type PortRange struct{ Low, High int }

func (r PortRange) String() string { return fmt.Sprintf("%d-%d", r.Low, r.High) }

// Contains reports whether port lies in the range.
func (r PortRange) Contains(port int) bool { return port >= r.Low && port <= r.High }

// ParsePortRange parses "low-high" (or a single port) with 1024 <= low <= high.
func ParsePortRange(s string) (PortRange, error) {
	lo, hi, found := strings.Cut(strings.TrimSpace(s), "-")
	if !found {
		hi = lo
	}
	low, err1 := strconv.Atoi(strings.TrimSpace(lo))
	high, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || low < 1024 || high > 65535 || low > high {
		return PortRange{}, fmt.Errorf("port range %q invalid (want low-high within 1024-65535)", s)
	}
	return PortRange{Low: low, High: high}, nil
}

// DynamicPortRange is the validated proxy_bridge.dynamic_ports.
func (pb ProxyBridgeConfig) DynamicPortRange() PortRange {
	r, err := ParsePortRange(pb.DynamicPorts)
	if err != nil {
		r, _ = ParsePortRange(defaultDynamicPorts)
	}
	return r
}

func (c *Config) validateEgress() error {
	switch c.Container.Egress {
	case "", EgressOpen, EgressRestricted:
	default:
		return fmt.Errorf("container.egress must be %q or %q", EgressOpen, EgressRestricted)
	}
	if c.Container.Egress == EgressRestricted {
		n := c.Container.Network
		if n == "host" || strings.HasPrefix(n, "container:") {
			return fmt.Errorf("container.egress=restricted requires an isolated Docker network, not %q", n)
		}
	}
	if c.ProxyBridge.DynamicPorts != "" {
		if _, err := ParsePortRange(c.ProxyBridge.DynamicPorts); err != nil {
			return fmt.Errorf("proxy_bridge.dynamic_ports: %w", err)
		}
	}
	return nil
}
