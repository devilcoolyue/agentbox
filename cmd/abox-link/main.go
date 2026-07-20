// abox-link runs on a user's own machine and gives their agentbox containers a
// way back into the LAN/intranet. It dials the server's /api/tunnel WebSocket,
// then services proxied connections the server pushes down the tunnel — each
// one checked against a default-deny whitelist and dialed locally, so the
// container reaches whatever this machine can reach (and nothing else).
//
// Usage:
//
//	abox-link --server https://box.example.com --user alice \
//	          --allow 192.168.1.0/24 --allow db.corp.local:5432 \
//	          --map 3306=10.0.1.5:3306
//
// --map additionally exposes a fixed TCP port on the server (bound on the
// docker gateway, reachable only by this user's containers) that pipes straight
// to one intranet target — for clients that cannot speak SOCKS (psql, mysql,
// redis-cli, database drivers). Mapped targets are implicitly whitelisted.
//
// Password is read from ABOX_PASSWORD (or --password, or interactively).
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"agentbox/internal/tunnel"
)

// errAuthRejected marks a WebSocket handshake rejected for auth reasons (the
// token was revoked or expired — e.g. the user changed their password, which
// revokes all tokens). The reconnect loop re-logs in when it can.
var errAuthRejected = errors.New("authentication rejected")

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	var (
		server   = flag.String("server", "", "agentbox base URL, e.g. https://box.example.com")
		user     = flag.String("user", "", "agentbox username")
		password = flag.String("password", "", "password (or set ABOX_PASSWORD)")
		token    = flag.String("token", "", "session token (skips login)")
		insecure = flag.Bool("insecure", false, "skip TLS certificate verification")
		allow    stringList
		mapFlags stringList
	)
	flag.Var(&allow, "allow", "allowed target: CIDR, host, or host:port (repeatable; default-deny)")
	flag.Var(&mapFlags, "map", "expose a fixed server port for one intranet target: PORT=HOST:PORT, e.g. 3306=10.0.1.5:3306 (repeatable)")
	flag.Parse()

	if *server == "" {
		fatal("--server is required")
	}
	if *token == "" && *user == "" {
		fatal("--user is required (or pass --token)")
	}
	var maps []tunnel.MapSpec
	for _, m := range mapFlags {
		spec, err := tunnel.ParseMapSpec(m)
		if err != nil {
			fatal(err.Error())
		}
		maps = append(maps, spec)
		allow = append(allow, spec.Target) // a mapped target is implicitly allowed
	}
	if len(allow) == 0 {
		fatal("at least one --allow or --map rule is required (default-deny); use --allow 0.0.0.0/0 to allow all")
	}
	wl, err := tunnel.ParseWhitelist(allow)
	if err != nil {
		fatal(err.Error())
	}
	mapsHdr := tunnel.EncodeMapSpecs(maps)

	base, err := url.Parse(*server)
	if err != nil || base.Host == "" {
		fatal("invalid --server URL")
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: *insecure}
	l := &tunnel.Link{Whitelist: wl, Logf: log.Printf}

	// Credentials: a token (from --token) or a username+password we can use to
	// (re)login. Password comes from --password or ABOX_PASSWORD.
	pw := *password
	if pw == "" {
		pw = os.Getenv("ABOX_PASSWORD")
	}
	tok := *token
	if tok == "" {
		if pw == "" {
			fatal("no password: set --password or ABOX_PASSWORD (or pass --token)")
		}
		tok, err = login(base, *user, pw, tlsCfg)
		if err != nil {
			fatal("login failed: " + err.Error())
		}
		log.Printf("logged in as %s", *user)
	}

	wsURL := wsURLFor(base)
	log.Printf("connecting tunnel to %s (%d allow rules, %d port maps)", wsURL, len(wl), len(maps))

	backoff := time.Second
	for {
		start := time.Now()
		err := serveOnce(l, wsURL, tok, mapsHdr, tlsCfg)
		if err != nil {
			log.Printf("tunnel closed: %v", err)
		}

		// Token was rejected: it is useless to retry the same one. Re-login if
		// we have a password; otherwise stop with a clear message rather than
		// spin forever on a dead token.
		if errors.Is(err, errAuthRejected) {
			if pw == "" {
				fatal("token rejected and no password to re-login; restart with --password or ABOX_PASSWORD")
			}
			newTok, lerr := login(base, *user, pw, tlsCfg)
			if lerr != nil {
				log.Printf("re-login failed: %v", lerr)
			} else {
				tok = newTok
				log.Printf("re-authenticated as %s", *user)
				backoff = time.Second
				continue
			}
		}

		// A connection that stayed up a while resets the backoff.
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// serveOnce dials the tunnel and services streams until the connection drops.
func serveOnce(l *tunnel.Link, wsURL, token, mapsHdr string, tlsCfg *tls.Config) error {
	dialer := websocket.Dialer{
		TLSClientConfig:  tlsCfg,
		HandshakeTimeout: 15 * time.Second,
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	if mapsHdr != "" {
		hdr.Set(tunnel.MapsHeader, mapsHdr)
	}
	conn, resp, err := dialer.Dial(wsURL, hdr)
	if err != nil {
		if resp != nil {
			detail := respDetail(resp)
			// 401 = token revoked/expired: re-login helps. 403 (e.g. the tunnel
			// feature is switched off server-side) would not be fixed by a new
			// token — surface the server's reason and keep the plain backoff.
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("dial %s: %s%s: %w", wsURL, resp.Status, detail, errAuthRejected)
			}
			return fmt.Errorf("dial %s: %s%s", wsURL, resp.Status, detail)
		}
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	logMapStatus(resp)
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	sess, err := yamux.Server(tunnel.NewWSConn(conn), cfg)
	if err != nil {
		_ = conn.Close()
		return err
	}
	log.Printf("tunnel established")
	return l.Serve(sess)
}

// respDetail pulls a short reason out of a failed handshake response body
// (the server sends {"error": "..."}).
func respDetail(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var out struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &out) == nil && out.Error != "" {
		return " (" + out.Error + ")"
	}
	return ""
}

// logMapStatus reports the server's per-map bind results from the handshake.
func logMapStatus(resp *http.Response) {
	if resp == nil {
		return
	}
	st := resp.Header.Get(tunnel.MapStatusHeader)
	if st == "" {
		return
	}
	for _, ent := range strings.Split(st, ",") {
		port, status, _ := strings.Cut(ent, "=")
		if status == "ok" {
			log.Printf("map %s: active on server", port)
		} else {
			log.Printf("map %s: FAILED on server: %s", port, status)
		}
	}
}

// --- server auth / URL helpers ---

func login(base *url.URL, user, pw string, tlsCfg *tls.Config) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/login"
	cl := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	resp, err := cl.Post(u.String(), "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(bufio.NewReader(resp.Body))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("no token in response")
	}
	return out.Token, nil
}

func wsURLFor(base *url.URL) string {
	scheme := "wss"
	if base.Scheme == "http" {
		scheme = "ws"
	}
	u := url.URL{Scheme: scheme, Host: base.Host, Path: strings.TrimRight(base.Path, "/") + "/api/tunnel"}
	return u.String()
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "abox-link: "+msg)
	os.Exit(1)
}
