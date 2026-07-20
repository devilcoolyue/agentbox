package tunnel

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// Rule is one whitelist entry: a CIDR, or a host with an optional port
// constraint. A target is allowed if any rule matches.
type Rule struct {
	CIDR *net.IPNet // set for CIDR rules
	Host string     // lowercased; set for host rules
	Port string     // optional port constraint on a host rule
}

// Whitelist is an ordered set of allow rules; empty means allow nothing.
type Whitelist []Rule

// ParseWhitelist turns "--allow" strings (CIDR, host, or host:port) into rules.
func ParseWhitelist(entries []string) (Whitelist, error) {
	var rules Whitelist
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, cidr, err := net.ParseCIDR(e); err == nil {
			rules = append(rules, Rule{CIDR: cidr})
			continue
		}
		if host, port, err := net.SplitHostPort(e); err == nil {
			rules = append(rules, Rule{Host: strings.ToLower(host), Port: port})
			continue
		}
		rules = append(rules, Rule{Host: strings.ToLower(e)})
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("no valid allow rules")
	}
	return rules, nil
}

// resolveAndCheck decides whether host:port is allowed and returns the concrete
// address to dial. The name is resolved at most once here; when a CIDR rule
// matches, the returned address pins the specific validated IP so a second
// resolution at dial time cannot swap in a different (unvetted) address —
// closing a DNS-rebinding gap. A host-name rule is name-based, so it returns
// host:port and dialing may re-resolve the name (consistent with the rule).
// lookupIP is injectable so tests need no real DNS.
func (w Whitelist) resolveAndCheck(host, port string, lookupIP func(string) ([]net.IP, error)) (bool, string) {
	hostL := strings.ToLower(host)

	needIPs := false
	for _, r := range w {
		if r.CIDR != nil {
			needIPs = true
			break
		}
	}
	var ips []net.IP
	if needIPs {
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else if lookupIP != nil {
			if resolved, err := lookupIP(host); err == nil {
				ips = resolved
			}
		}
	}

	for _, r := range w {
		if r.CIDR != nil {
			for _, ip := range ips {
				if r.CIDR.Contains(ip) {
					return true, net.JoinHostPort(ip.String(), port)
				}
			}
			continue
		}
		if r.Host == hostL && (r.Port == "" || r.Port == port) {
			return true, net.JoinHostPort(host, port)
		}
	}
	return false, ""
}

// Allowed reports whether host:port is permitted. Thin predicate wrapper over
// resolveAndCheck for callers that only need the yes/no.
func (w Whitelist) Allowed(host, port string, lookupIP func(string) ([]net.IP, error)) bool {
	ok, _ := w.resolveAndCheck(host, port, lookupIP)
	return ok
}

// Link services the streams a server pushes down a tunnel: it enforces the
// whitelist and dials allowed targets on the local machine.
type Link struct {
	Whitelist Whitelist
	// Logf logs one audit line per connection; nil disables logging.
	Logf func(format string, args ...any)
	// DialTimeout bounds each outbound dial (default 10s).
	DialTimeout time.Duration
}

func (l *Link) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

// Serve accepts streams from sess until it closes, returning the terminating
// error.
func (l *Link) Serve(sess *yamux.Session) error {
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			_ = sess.Close()
			return err
		}
		go l.serveStream(stream)
	}
}

func (l *Link) serveStream(stream *yamux.Stream) {
	defer stream.Close()
	typ, addr, err := ReadRequest(stream)
	if err != nil || typ != StreamConnect {
		return
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		_, _ = stream.Write([]byte{StatusDialError})
		return
	}
	ok, dialAddr := l.Whitelist.resolveAndCheck(host, port, net.LookupIP)
	if !ok {
		l.logf("DENY  %s (not in whitelist)", addr)
		_, _ = stream.Write([]byte{StatusDenied})
		return
	}
	timeout := l.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	// Resolution and dial happen here, on this machine — that is what lets the
	// container reach names/hosts only visible from the local network. dialAddr
	// pins the validated IP for CIDR matches (no re-resolution).
	target, err := net.DialTimeout("tcp", dialAddr, timeout)
	if err != nil {
		l.logf("FAIL  %s: %v", addr, err)
		_, _ = stream.Write([]byte{StatusDialError})
		return
	}
	defer target.Close()
	l.logf("OPEN  %s -> %s", addr, target.RemoteAddr())
	if _, err := stream.Write([]byte{StatusOK}); err != nil {
		return
	}
	Pipe(stream, target)
	l.logf("CLOSE %s", addr)
}
