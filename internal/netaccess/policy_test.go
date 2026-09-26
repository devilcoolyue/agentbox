package netaccess

import (
	"net/netip"
	"testing"
)

func TestPolicyIsolationAndRevocation(t *testing.T) {
	p, err := Merge(Policy{}, []string{"db.corp:5432", "10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Captures("db.corp") || !p.Captures("10.20.1.5") || p.Captures("10.21.0.1") {
		t.Fatal(p)
	}
	if p.Allows("db.corp:80") || !p.Allows("db.corp:5432") {
		t.Fatal("port restriction lost")
	}
	fake := p.Domains["db.corp"]
	p, err = Merge(p, []string{"other.corp"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Allows("db.corp:5432") || !p.Captures("db.corp") || p.Domains["db.corp"] != fake {
		t.Fatal("revocation lost capture or recycled IP")
	}
	target, err := p.Target(fake + ":5432")
	if err != nil || target != "db.corp:5432" {
		t.Fatal(target, err)
	}
	if _, err = p.Target("198.19.255.255:80"); err == nil {
		t.Fatal("unknown fake IP accepted")
	}
	other, _ := Merge(Policy{}, []string{"10.30.0.0/16"})
	if other.Captures("10.20.1.5") {
		t.Fatal("user policy leaked")
	}
}
func TestPolicyValidation(t *testing.T) {
	for _, s := range []string{"0.0.0.0/0", "127.0.0.1", "localhost:80", "169.254.1.1", "198.18.1.1", "::1", "2001:db8::/32", "foo;touch bad", "*.corp", "db.corp:0", "10.2.3.4/99"} {
		if _, err := Merge(Policy{}, []string{s}); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	p, _ := Merge(Policy{}, []string{"10.20.0.0/16"})
	if p.CheckConflicts([]netip.Prefix{netip.MustParsePrefix("10.20.1.0/24")}) == nil {
		t.Fatal("missed bridge conflict")
	}
}
