package tunnel

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// HTTP headers on the /api/tunnel WebSocket handshake that carry the link's
// port-map declaration and the server's per-map bind results.
const (
	MapsHeader      = "X-Abox-Maps"       // request: EncodeMapSpecs form
	MapStatusHeader = "X-Abox-Map-Status" // response: "3306=ok,6379=<error>"
)

// MapSpec declares one fixed port-forward: the server listens on Port (on the
// same host the SOCKS proxy binds, i.e. the docker gateway) and pipes every
// accepted connection down the tunnel to Target, dialed on the link machine.
// This gives clients that cannot speak SOCKS (psql, mysql, redis-cli, app
// drivers) a plain TCP path into the intranet.
type MapSpec struct {
	Port   int    // listen port on the server's proxy host
	Target string // intranet "host:port" the link dials
}

// ParseMapSpec parses one "--map" value: "PORT=HOST:PORT".
func ParseMapSpec(s string) (MapSpec, error) {
	portStr, target, ok := strings.Cut(strings.TrimSpace(s), "=")
	if !ok {
		return MapSpec{}, fmt.Errorf("map %q: want PORT=HOST:PORT", s)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return MapSpec{}, fmt.Errorf("map %q: bad listen port %q", s, portStr)
	}
	host, tport, err := net.SplitHostPort(target)
	if err != nil || host == "" || tport == "" {
		return MapSpec{}, fmt.Errorf("map %q: target %q is not host:port", s, target)
	}
	return MapSpec{Port: port, Target: target}, nil
}

// EncodeMapSpecs joins specs into the comma-separated header form.
func EncodeMapSpecs(specs []MapSpec) string {
	parts := make([]string, len(specs))
	for i, m := range specs {
		parts[i] = strconv.Itoa(m.Port) + "=" + m.Target
	}
	return strings.Join(parts, ",")
}

// ParseMapSpecs parses the comma-separated header form (inverse of
// EncodeMapSpecs). An empty string yields no specs.
func ParseMapSpecs(s string) ([]MapSpec, error) {
	var specs []MapSpec
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		m, err := ParseMapSpec(part)
		if err != nil {
			return nil, err
		}
		specs = append(specs, m)
	}
	return specs, nil
}
