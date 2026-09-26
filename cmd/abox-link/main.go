// abox-link runs on a user's own machine and gives their agentbox containers a
// way back into the LAN/intranet. It dials the server's /api/tunnel WebSocket,
// then services proxied connections the server pushes down the tunnel — each
// one checked against a default-deny whitelist and dialed locally, so the
// container reaches whatever this machine can reach (and nothing else).
//
// # Two ways to run it
//
// Run it with no arguments and it opens a local control panel in the browser:
// paste a pairing code from agentbox, add allow rules by clicking, press start,
// and optionally have it launch at boot. No flags, no terminal.
//
//	abox-link                     # control panel on http://127.0.0.1:7801
//	abox-link --daemon            # same, no browser (used by the autostart service)
//
// Passing --server instead keeps the original headless behaviour, for servers
// and scripts:
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
// In flag mode the password is read from ABOX_PASSWORD (or --password).
package main

import (
	"agentbox/internal/buildinfo"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"agentbox/internal/linkapp"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Println(buildinfo.String("abox-link"))
		return
	}
	log.SetFlags(log.LstdFlags)

	var (
		server      = flag.String("server", "", "agentbox base URL, e.g. https://box.example.com")
		user        = flag.String("user", "", "agentbox username")
		password    = flag.String("password", "", "password (or set ABOX_PASSWORD)")
		token       = flag.String("token", "", "session token (skips login)")
		transparent = flag.Bool("transparent", true, "transparent TCP intranet access (default; use --transparent=false for legacy proxy mode, not recommended)")
		insecure    = flag.Bool("insecure", false, "skip TLS certificate verification")
		daemon      = flag.Bool("daemon", false, "run the control panel without opening a browser")
		addr        = flag.String("addr", linkapp.DefaultAddr, "control panel listen address (loopback only)")
		allow       stringList
		mapFlags    stringList
	)
	flag.Var(&allow, "allow", "allowed target: CIDR, host, or host:port (repeatable; default-deny)")
	flag.Var(&mapFlags, "map", "expose a fixed server port for one intranet target: PORT=HOST:PORT, e.g. 3306=10.0.1.5:3306 (repeatable)")
	flag.Parse()

	// --server is what separates "I am scripting this" from "I just ran the
	// app": every other setting has a default the panel can supply itself.
	if *server == "" {
		if err := linkapp.RunPanel(linkapp.RunOptions{Addr: *addr, Daemon: *daemon}); err != nil {
			fatal(err.Error())
		}
		return
	}
	runHeadless(*server, *user, *password, *token, *insecure, allow, mapFlags, *transparent)
}

// runHeadless is the original flag-driven mode: build a config from the command
// line, connect, and stay in the foreground printing the audit log.
func runHeadless(server, user, password, token string, insecure bool, allow, maps stringList, transparent bool) {
	if token == "" && user == "" {
		fatal("--user is required (or pass --token)")
	}
	if len(allow) == 0 && len(maps) == 0 {
		fatal("at least one --allow or --map rule is required (default-deny); use --allow 0.0.0.0/0 to allow all")
	}

	pw := password
	if pw == "" {
		pw = os.Getenv("ABOX_PASSWORD")
	}
	if token == "" && pw == "" {
		fatal("no password: set --password or ABOX_PASSWORD (or pass --token)")
	}

	cfg := linkapp.Config{
		Transparent: transparent,
		Server:      server,
		User:        user,
		Token:       token,
		Insecure:    insecure,
		Allow:       allow,
		Maps:        maps,
	}
	if _, _, err := cfg.Validate(); err != nil {
		fatal(err.Error())
	}

	if cfg.Token == "" {
		tok, err := linkapp.Login(cfg.Server, cfg.User, pw, cfg.Insecure)
		if err != nil {
			fatal("login failed: " + err.Error())
		}
		cfg.Token = tok
		log.Printf("logged in as %s", cfg.User)
	}

	sup := linkapp.NewSupervisor()
	sup.MirrorTo(func(line string) { log.Print(line) })
	// A revoked token is recoverable here because we still hold the password.
	if pw != "" {
		sup.Relogin = func() (string, error) {
			return linkapp.Login(cfg.Server, cfg.User, pw, cfg.Insecure)
		}
	}
	if err := sup.Start(cfg); err != nil {
		fatal(err.Error())
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	sup.Stop()
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "abox-link: "+msg)
	os.Exit(1)
}
