package netaccess

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// EgressPolicy is the restricted-egress allowlist for one workspace network
// namespace: TCP to Hosts on Ports (and the inclusive dynamic range) is all a
// workspace may open. Everything else — internet, other host services, other
// workspaces — is rejected, and new inbound connections are dropped.
//
// The rules apply to every UID in the namespace, including the transparent
// network helper: its intranet traffic goes to the control port (allowed), and
// its direct fallback dials are meant to fail closed under this policy.
type EgressPolicy struct {
	Hosts     []netip.Addr
	Ports     []int
	RangeLow  int
	RangeHigh int
}

// Chains installed in the workspace namespace's filter table.
const (
	EgressChain  = "ABX_EGRESS"
	IngressChain = "ABX_INGRESS"
)

// multiport accepts at most 15 port slots; a range takes two.
const maxMultiport = 15

func (p EgressPolicy) normalized() (EgressPolicy, error) {
	seenHost := map[netip.Addr]bool{}
	var hosts []netip.Addr
	for _, h := range p.Hosts {
		h = h.Unmap()
		if !h.Is4() || h.IsUnspecified() {
			return p, fmt.Errorf("egress host %s must be a specific IPv4 address", h)
		}
		if !seenHost[h] {
			seenHost[h] = true
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return p, fmt.Errorf("egress policy has no reachable agentbox host")
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Less(hosts[j]) })
	seenPort := map[int]bool{}
	var ports []int
	for _, port := range p.Ports {
		if port < 1 || port > 65535 {
			return p, fmt.Errorf("egress port %d out of range", port)
		}
		if p.RangeLow > 0 && port >= p.RangeLow && port <= p.RangeHigh {
			continue
		}
		if !seenPort[port] {
			seenPort[port] = true
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	slots := len(ports)
	if p.RangeLow > 0 {
		if p.RangeLow > p.RangeHigh || p.RangeHigh > 65535 {
			return p, fmt.Errorf("egress port range %d-%d invalid", p.RangeLow, p.RangeHigh)
		}
		slots += 2
	}
	if slots == 0 {
		return p, fmt.Errorf("egress policy allows no ports")
	}
	if slots > maxMultiport {
		return p, fmt.Errorf("egress policy has too many ports (%d slots, max %d)", slots, maxMultiport)
	}
	return EgressPolicy{Hosts: hosts, Ports: ports, RangeLow: p.RangeLow, RangeHigh: p.RangeHigh}, nil
}

// Rules renders iptables-restore (IPv4) and ip6tables-restore (IPv6) input.
// Both declare and flush their own chains, so reapplying swaps the whole
// policy atomically; the OUTPUT/INPUT jumps are added separately (see
// EgressScript) because iptables-restore --noflush cannot add them
// idempotently.
func (p EgressPolicy) Rules() (v4, v6 string, err error) {
	if p, err = p.normalized(); err != nil {
		return "", "", err
	}
	var slots []string
	for _, port := range p.Ports {
		slots = append(slots, strconv.Itoa(port))
	}
	if p.RangeLow > 0 {
		slots = append(slots, fmt.Sprintf("%d:%d", p.RangeLow, p.RangeHigh))
	}
	dports := strings.Join(slots, ",")
	var b strings.Builder
	b.WriteString("*filter\n:" + EgressChain + " - [0:0]\n:" + IngressChain + " - [0:0]\n")
	b.WriteString("-F " + EgressChain + "\n-F " + IngressChain + "\n")
	b.WriteString("-A " + EgressChain + " -o lo -j RETURN\n")
	b.WriteString("-A " + EgressChain + " -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n")
	for _, h := range p.Hosts {
		b.WriteString("-A " + EgressChain + " -d " + h.String() + "/32 -p tcp -m multiport --dports " + dports + " -j RETURN\n")
	}
	b.WriteString("-A " + EgressChain + " -p tcp -j REJECT --reject-with tcp-reset\n")
	b.WriteString("-A " + EgressChain + " -j REJECT --reject-with icmp-admin-prohibited\n")
	b.WriteString("-A " + IngressChain + " -i lo -j RETURN\n")
	b.WriteString("-A " + IngressChain + " -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n")
	b.WriteString("-A " + IngressChain + " -j DROP\nCOMMIT\n")
	v4 = b.String()
	// No IPv6 endpoint is ever advertised to workspaces: loopback only.
	v6 = "*filter\n:" + EgressChain + " - [0:0]\n:" + IngressChain + " - [0:0]\n" +
		"-F " + EgressChain + "\n-F " + IngressChain + "\n" +
		"-A " + EgressChain + " -o lo -j RETURN\n" +
		"-A " + EgressChain + " -j REJECT\n" +
		"-A " + IngressChain + " -i lo -j RETURN\n" +
		"-A " + IngressChain + " -j DROP\nCOMMIT\n"
	return v4, v6, nil
}

// EgressScript runs in a one-shot NET_ADMIN container that shares the
// workspace network namespace. It reads the rules from ABX_EGRESS_V4/V6,
// installs the OUTPUT/INPUT jumps once, and verifies them. IPv6 failures are
// tolerated only when the namespace has no global IPv6 address (Docker's
// default bridge disables IPv6 in containers, which can make ip6tables fail).
const EgressScript = `set -eu
printf '%s' "$ABX_EGRESS_V4" | iptables-restore -w 5 --noflush
iptables -w 5 -C OUTPUT -j ` + EgressChain + ` 2>/dev/null || iptables -w 5 -I OUTPUT 1 -j ` + EgressChain + `
iptables -w 5 -C INPUT -j ` + IngressChain + ` 2>/dev/null || iptables -w 5 -I INPUT 1 -j ` + IngressChain + `
if out=$(printf '%s' "$ABX_EGRESS_V6" | ip6tables-restore -w 5 --noflush 2>&1); then
  ip6tables -w 5 -C OUTPUT -j ` + EgressChain + ` 2>/dev/null || ip6tables -w 5 -I OUTPUT 1 -j ` + EgressChain + `
  ip6tables -w 5 -C INPUT -j ` + IngressChain + ` 2>/dev/null || ip6tables -w 5 -I INPUT 1 -j ` + IngressChain + `
elif [ -n "$(ip -6 addr show scope global 2>/dev/null)" ]; then
  echo "ipv6 egress rules: $out" >&2
  exit 1
fi
iptables -w 5 -S OUTPUT | grep -q -- "-j ` + EgressChain + `"
iptables -w 5 -S INPUT | grep -q -- "-j ` + IngressChain + `"
`
