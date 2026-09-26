// Package netaccess implements transparent TCP access for workspace networks.
// Policies select routes; authorization is always enforced again by abox-link.
package netaccess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

const (
	Protocol         = "transparent-v1"
	RulesHeader      = "X-Agentbox-Network-Rules"
	CapabilityHeader = "X-Agentbox-Network"
	FakeCIDR         = "198.18.0.0/15"
	TCPPort          = 15001
	DNSPort          = 15053
	MaxRules         = 128
	MaxHistory       = 1024
)

// Policy is persisted per user, including retired destinations. Retired routes
// remain captured (and denied) so stale DNS caches cannot reach a different LAN.
// Domain addresses are never recycled within the policy's lifetime.
type Policy struct {
	Rules    []string          `json:"rules"`
	Routes   []string          `json:"routes"`
	Domains  map[string]string `json:"domains"`
	Revision string            `json:"revision"`
}

func NormalizeRules(entries []string) ([]string, error) {
	if len(entries) > MaxRules {
		return nil, fmt.Errorf("最多配置 %d 条透明访问规则", MaxRules)
	}
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		host, port, err := splitRule(entry)
		if err != nil {
			return nil, err
		}
		value := host
		if port != "" {
			value = net.JoinHostPort(host, port)
		}
		if !seen[value] {
			out = append(out, value)
			seen[value] = true
		}
	}
	sort.Strings(out)
	return out, nil
}

func splitRule(entry string) (string, string, error) {
	s := strings.ToLower(strings.TrimSpace(entry))
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return "", "", fmt.Errorf("无效端口: %q", entry)
		}
		port = strconv.Itoa(n)
	}
	if p, err := netip.ParsePrefix(host); err == nil {
		if port != "" || !p.Addr().Is4() {
			return "", "", fmt.Errorf("透明访问第一版仅支持 IPv4 网段: %q", entry)
		}
		p = p.Masked()
		for _, reserved := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", FakeCIDR, "224.0.0.0/3"} {
			r := netip.MustParsePrefix(reserved)
			if p.Overlaps(r) {
				return "", "", fmt.Errorf("网段与保留地址冲突: %q", entry)
			}
		}
		return p.String(), "", nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || netip.MustParsePrefix(FakeCIDR).Contains(ip) || ip.As4()[0] == 0 {
			return "", "", fmt.Errorf("不支持的透明访问地址: %q", entry)
		}
		return ip.String(), port, nil
	}
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", "", fmt.Errorf("无效内网域名: %q", entry)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", "", fmt.Errorf("无效内网域名: %q", entry)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", "", fmt.Errorf("无效内网域名: %q", entry)
			}
		}
	}
	return host, port, nil
}

// Merge retains old capture destinations, while replacing current grants.
func Merge(old Policy, entries []string) (Policy, error) {
	rules, err := NormalizeRules(entries)
	if err != nil {
		return Policy{}, err
	}
	p := Policy{Rules: rules, Routes: append([]string{}, old.Routes...), Domains: map[string]string{}}
	for k, v := range old.Domains {
		p.Domains[k] = v
	}
	seen := map[string]bool{}
	for _, r := range p.Routes {
		seen[r] = true
	}
	for _, r := range rules {
		host, _, _ := splitRule(r)
		if !seen[host] {
			p.Routes = append(p.Routes, host)
			seen[host] = true
		}
		if _, e := netip.ParsePrefix(host); e == nil {
			continue
		}
		if _, e := netip.ParseAddr(host); e == nil {
			continue
		}
		if _, ok := p.Domains[host]; !ok {
			n := len(p.Domains) + 1
			p.Domains[host] = fmt.Sprintf("198.%d.%d.%d", 18+n/65536, (n/256)%256, n%256)
		}
	}
	if len(p.Routes) > MaxHistory {
		return Policy{}, fmt.Errorf("透明访问历史目标超过 %d 条，请联系管理员", MaxHistory)
	}
	sort.Strings(p.Routes)
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	p.Revision = hex.EncodeToString(sum[:])[:16]
	return p, nil
}

// Captures intentionally ignores port restrictions: disallowed ports must fail
// at the tunnel, never fall through to a similarly addressed server-side LAN.
func (p Policy) Captures(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	ip, err := netip.ParseAddr(host)
	if err == nil && netip.MustParsePrefix(FakeCIDR).Contains(ip) {
		return true
	}
	for _, r := range p.Routes {
		if host == r {
			return true
		}
		if prefix, e := netip.ParsePrefix(r); e == nil && err == nil && prefix.Contains(ip) {
			return true
		}
	}
	return false
}
func (p Policy) Target(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	ip, err := netip.ParseAddr(host)
	if err == nil && netip.MustParsePrefix(FakeCIDR).Contains(ip) {
		for domain, fake := range p.Domains {
			if fake == host {
				return net.JoinHostPort(domain, port), nil
			}
		}
		return "", fmt.Errorf("unknown virtual address")
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.ToLower(host), "."), port), nil
}
func (p Policy) Allows(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	ip, ipErr := netip.ParseAddr(host)
	for _, r := range p.Rules {
		h, po, e := splitRule(r)
		if e != nil || po != "" && po != port {
			continue
		}
		if h == host {
			return true
		}
		if prefix, e := netip.ParsePrefix(h); e == nil && ipErr == nil && prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// CheckConflicts rejects routes overlapping the workspace's actual interfaces.
func (p Policy) CheckConflicts(networks []netip.Prefix) error {
	for _, r := range p.Routes {
		prefix, err := netip.ParsePrefix(r)
		if err != nil {
			if ip, e := netip.ParseAddr(r); e == nil {
				prefix = netip.PrefixFrom(ip, 32)
			} else {
				continue
			}
		}
		for _, network := range networks {
			if prefix.Overlaps(network) {
				return fmt.Errorf("内网目标 %s 与工作空间网络 %s 冲突", r, network)
			}
		}
	}
	for _, network := range networks {
		if netip.MustParsePrefix(FakeCIDR).Overlaps(network) {
			return fmt.Errorf("虚拟地址段与工作空间网络 %s 冲突", network)
		}
	}
	return nil
}
