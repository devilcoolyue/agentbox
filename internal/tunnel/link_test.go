package tunnel

import (
	"net"
	"testing"
)

func TestResolveAndCheckPinsCIDRMatch(t *testing.T) {
	wl, err := ParseWhitelist([]string{"10.0.0.0/8", "gitlab.corp.local"})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) ([]net.IP, error) {
		switch name {
		case "db.corp.local":
			return []net.IP{net.ParseIP("10.1.2.3")}, nil
		case "gitlab.corp.local":
			return []net.IP{net.ParseIP("172.16.0.9")}, nil // not in any CIDR
		}
		return nil, net.UnknownNetworkError(name)
	}

	// Name resolving into the CIDR: allowed, and the dial target is pinned to
	// the exact validated IP (not the name) to defeat DNS rebinding.
	ok, dial := wl.resolveAndCheck("db.corp.local", "5432", lookup)
	if !ok || dial != "10.1.2.3:5432" {
		t.Fatalf("CIDR name match: ok=%v dial=%q, want true / 10.1.2.3:5432", ok, dial)
	}

	// Host-name rule: allowed by name, dial keeps the name (rule is name-based).
	ok, dial = wl.resolveAndCheck("gitlab.corp.local", "443", lookup)
	if !ok || dial != "gitlab.corp.local:443" {
		t.Fatalf("host rule match: ok=%v dial=%q, want true / gitlab.corp.local:443", ok, dial)
	}

	// A literal IP in range pins itself.
	ok, dial = wl.resolveAndCheck("10.9.9.9", "80", lookup)
	if !ok || dial != "10.9.9.9:80" {
		t.Fatalf("literal IP: ok=%v dial=%q", ok, dial)
	}

	// Not allowed.
	if ok, _ := wl.resolveAndCheck("evil.com", "80", lookup); ok {
		t.Fatal("evil.com should be denied")
	}
}
