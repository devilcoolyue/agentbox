package netaccess

import (
	"net/netip"
	"strings"
	"testing"
)

func TestEgressRules(t *testing.T) {
	gw := netip.MustParseAddr("172.17.0.1")
	p := EgressPolicy{Hosts: []netip.Addr{gw, gw}, Ports: []int{1083, 1081, 1081, 30005}, RangeLow: 30000, RangeHigh: 30999}
	v4, v6, err := p.Rules()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		":ABX_EGRESS - [0:0]", "-F ABX_EGRESS",
		"-A ABX_EGRESS -o lo -j RETURN",
		"-A ABX_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN",
		"-A ABX_EGRESS -d 172.17.0.1/32 -p tcp -m multiport --dports 1081,1083,30000:30999 -j RETURN",
		"-A ABX_EGRESS -p tcp -j REJECT --reject-with tcp-reset",
		"-A ABX_INGRESS -j DROP",
	} {
		if !strings.Contains(v4, want) {
			t.Errorf("v4 missing %q:\n%s", want, v4)
		}
	}
	if strings.Count(v4, "multiport") != 1 {
		t.Errorf("duplicate host not folded:\n%s", v4)
	}
	if strings.Index(v4, "-d 172.17.0.1") > strings.Index(v4, "REJECT") {
		t.Error("allow rule must precede the reject")
	}
	if strings.Contains(v6, "multiport") || !strings.Contains(v6, "-A ABX_EGRESS -j REJECT") {
		t.Errorf("v6 must be loopback-only:\n%s", v6)
	}
}

func TestEgressRulesRejectBadPolicies(t *testing.T) {
	gw := netip.MustParseAddr("172.17.0.1")
	many := make([]int, 14)
	for i := range many {
		many[i] = 2000 + i
	}
	for name, p := range map[string]EgressPolicy{
		"no hosts":  {Ports: []int{1081}},
		"wildcard":  {Hosts: []netip.Addr{netip.MustParseAddr("0.0.0.0")}, Ports: []int{1081}},
		"ipv6 host": {Hosts: []netip.Addr{netip.MustParseAddr("fd00::1")}, Ports: []int{1081}},
		"no ports":  {Hosts: []netip.Addr{gw}},
		"bad port":  {Hosts: []netip.Addr{gw}, Ports: []int{70000}},
		"too many":  {Hosts: []netip.Addr{gw}, Ports: many, RangeLow: 30000, RangeHigh: 30999},
		"bad range": {Hosts: []netip.Addr{gw}, Ports: []int{1081}, RangeLow: 3000, RangeHigh: 2000},
	} {
		if _, _, err := p.Rules(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEgressScriptInstallsJumpsOnce(t *testing.T) {
	for _, want := range []string{
		`iptables-restore -w 5 --noflush`,
		`iptables -w 5 -C OUTPUT -j ABX_EGRESS 2>/dev/null || iptables -w 5 -I OUTPUT 1 -j ABX_EGRESS`,
		`iptables -w 5 -C INPUT -j ABX_INGRESS 2>/dev/null || iptables -w 5 -I INPUT 1 -j ABX_INGRESS`,
		`ip -6 addr show scope global`,
	} {
		if !strings.Contains(EgressScript, want) {
			t.Errorf("script missing %q", want)
		}
	}
	if !strings.HasPrefix(EgressScript, "set -eu\n") {
		t.Error("script must stop on the first failure")
	}
}
