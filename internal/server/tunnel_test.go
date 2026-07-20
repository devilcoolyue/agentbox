package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/net/proxy"

	"agentbox/internal/tunnel"
)

// wireLink connects a fake link to the hub over an in-memory pipe: the server
// end becomes the yamux client (registered under user), the link end runs the
// real tunnel.Link with the given whitelist. Returns the user's secret.
func wireLink(t *testing.T, hub *tunnelHub, user string, wl tunnel.Whitelist) string {
	t.Helper()
	srvEnd, linkEnd := net.Pipe()

	serverSess, err := yamux.Client(srvEnd, yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("yamux client: %v", err)
	}
	linkSess, err := yamux.Server(linkEnd, yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("yamux server: %v", err)
	}
	secret, unregister := hub.register(user, serverSess, "test:0")
	link := &tunnel.Link{Whitelist: wl}
	go link.Serve(linkSess)
	t.Cleanup(func() {
		unregister()
		serverSess.Close()
		linkSess.Close()
	})
	return secret
}

// startProxy binds the hub's SOCKS server on a loopback port for the test.
func startProxy(t *testing.T, hub *tunnelHub) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := hub.socksServer()
	go srv.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// getVia fetches url through the SOCKS proxy using user/pass auth.
func getVia(proxyAddr, user, pass, url string) (string, error) {
	auth := &proxy.Auth{User: user, Password: pass}
	d, err := proxy.SOCKS5("tcp", proxyAddr, auth, proxy.Direct)
	if err != nil {
		return "", err
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.Dial(network, addr)
	}}
	cl := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

func TestTunnelProxyEndToEnd(t *testing.T) {
	// A fake "intranet" service the link can reach.
	intr := httptestServer(t, "OK-INTRANET")
	_, port, _ := net.SplitHostPort(intr)

	hub := newTunnelHub()
	proxyAddr := startProxy(t, hub)

	wl, err := tunnel.ParseWhitelist([]string{
		"127.0.0.1:" + port, // exact endpoint
		"localhost:" + port, // name form, resolved link-side (socks5h)
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := wireLink(t, hub, "alice", wl)
	waitReady(t, hub, "alice")

	t.Run("ip through proxy", func(t *testing.T) {
		body, err := getVia(proxyAddr, "alice", secret, "http://"+intr+"/x")
		if err != nil || !strings.Contains(body, "OK-INTRANET") {
			t.Fatalf("got body=%q err=%v", body, err)
		}
	})

	// socks5h: the SOCKS server must forward "localhost" unresolved so the link
	// resolves it. Proves DNS happens link-side.
	t.Run("name resolves link-side", func(t *testing.T) {
		body, err := getVia(proxyAddr, "alice", secret, "http://localhost:"+port+"/x")
		if err != nil || !strings.Contains(body, "OK-INTRANET") {
			t.Fatalf("got body=%q err=%v", body, err)
		}
	})

	t.Run("wrong secret rejected", func(t *testing.T) {
		if _, err := getVia(proxyAddr, "alice", "wrong-secret", "http://"+intr+"/x"); err == nil {
			t.Fatal("expected auth failure, got success")
		}
	})

	t.Run("unknown user rejected", func(t *testing.T) {
		if _, err := getVia(proxyAddr, "mallory", secret, "http://"+intr+"/x"); err == nil {
			t.Fatal("expected auth failure for unknown user")
		}
	})
}

func TestTunnelWhitelistDenies(t *testing.T) {
	intr := httptestServer(t, "SECRET")
	hub := newTunnelHub()
	proxyAddr := startProxy(t, hub)

	// Whitelist allows only a different host, so the intranet target is denied.
	wl, _ := tunnel.ParseWhitelist([]string{"10.0.0.0/8"})
	secret := wireLink(t, hub, "bob", wl)
	waitReady(t, hub, "bob")

	if _, err := getVia(proxyAddr, "bob", secret, "http://"+intr+"/x"); err == nil {
		t.Fatal("expected whitelist denial, got success")
	}
}

func TestWhitelistMatching(t *testing.T) {
	lookup := func(name string) ([]net.IP, error) {
		if name == "db.corp.local" {
			return []net.IP{net.ParseIP("10.1.2.3")}, nil
		}
		return nil, fmt.Errorf("nxdomain")
	}
	wl, err := tunnel.ParseWhitelist([]string{"192.168.1.0/24", "gitlab.corp.local", "db.corp.local:5432"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host, port string
		want       bool
	}{
		{"192.168.1.50", "443", true},     // in CIDR
		{"192.168.2.50", "443", false},    // outside CIDR
		{"gitlab.corp.local", "22", true}, // host rule, any port
		{"GITLAB.CORP.LOCAL", "80", true}, // case-insensitive
		{"db.corp.local", "5432", true},   // host:port rule, exact port
		{"db.corp.local", "3306", false},  // right host, wrong port
		{"evil.com", "80", false},         // not listed
	}
	for _, c := range cases {
		got := wl.Allowed(c.host, c.port, lookup)
		if got != c.want {
			t.Errorf("Allowed(%s:%s)=%v want %v", c.host, c.port, got, c.want)
		}
	}
}

// TestTunnelPortMap exercises the fixed-port forward path: a raw TCP listener
// piped down the tunnel to one fixed target, gated by source-IP authorization.
func TestTunnelPortMap(t *testing.T) {
	intr := httptestServer(t, "OK-MAPPED")
	hub := newTunnelHub()

	wl, _ := tunnel.ParseWhitelist([]string{intr}) // the mapped target, as the link would auto-allow it
	wireLink(t, hub, "alice", wl)
	waitReady(t, hub, "alice")

	newMapListener := func(t *testing.T, user, target string, authorize func(user, srcIP string) bool) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go hub.serveMapListener(user, target, ln, authorize)
		return ln.Addr().String()
	}

	t.Run("allowed source relays to target", func(t *testing.T) {
		addr := newMapListener(t, "alice", intr, func(user, src string) bool {
			return user == "alice" && src == "127.0.0.1"
		})
		cl := &http.Client{Timeout: 5 * time.Second}
		resp, err := cl.Get("http://" + addr + "/x")
		if err != nil {
			t.Fatalf("GET via map: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "OK-MAPPED") {
			t.Fatalf("body=%q", body)
		}
	})

	t.Run("denied source is dropped", func(t *testing.T) {
		addr := newMapListener(t, "alice", intr, func(string, string) bool { return false })
		cl := &http.Client{Timeout: 2 * time.Second}
		if _, err := cl.Get("http://" + addr + "/x"); err == nil {
			t.Fatal("expected connection failure for denied source")
		}
	})

	t.Run("no live tunnel drops the connection", func(t *testing.T) {
		addr := newMapListener(t, "nobody", intr, func(string, string) bool { return true })
		cl := &http.Client{Timeout: 2 * time.Second}
		if _, err := cl.Get("http://" + addr + "/x"); err == nil {
			t.Fatal("expected failure without a live tunnel")
		}
	})

	t.Run("whitelist still applies to mapped target", func(t *testing.T) {
		other := httptestServer(t, "NOT-ALLOWED") // not in alice's whitelist
		addr := newMapListener(t, "alice", other, func(string, string) bool { return true })
		cl := &http.Client{Timeout: 2 * time.Second}
		if _, err := cl.Get("http://" + addr + "/x"); err == nil {
			t.Fatal("expected link-side whitelist denial")
		}
	})
}

// TestSetMapsGenerations checks that a stale connection's cleanup cannot tear
// down the maps a newer connection has bound.
func TestSetMapsGenerations(t *testing.T) {
	hub := newTunnelHub()
	mkEntry := func(t *testing.T, port int) mapEntry {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		return mapEntry{Port: port, Target: "10.0.0.1:1", ln: ln}
	}

	release1 := hub.setMaps("u", []mapEntry{mkEntry(t, 1111)})
	e2 := mkEntry(t, 2222)
	release2 := hub.setMaps("u", []mapEntry{e2}) // supersedes generation 1

	release1() // stale cleanup: must not remove generation 2
	got := hub.mapsFor("u")
	if len(got) != 1 || got[0].Port != 2222 {
		t.Fatalf("after stale release: maps=%+v, want the 2222 entry", got)
	}
	// Generation 2's listener must still accept.
	c, err := net.DialTimeout("tcp", e2.ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("gen-2 listener closed by stale release: %v", err)
	}
	c.Close()

	release2()
	if got := hub.mapsFor("u"); len(got) != 0 {
		t.Fatalf("after own release: maps=%+v, want none", got)
	}
}

// --- helpers ---

func httptestServer(t *testing.T, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func waitReady(t *testing.T, hub *tunnelHub, user string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, up := hub.proxyCreds(user); up {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tunnel for %s never became ready", user)
}
