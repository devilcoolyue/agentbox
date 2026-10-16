// abox-loop-guard is a standalone model SSE gateway; it needs neither Docker
// nor an Agentbox server or database.
package main

import (
	"agentbox/pkg/loopguard"
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8788", "HTTP listen address")
	upstream := flag.String("upstream", "", "fixed upstream origin/path prefix (required)")
	authHeader := flag.String("auth-header", "X-Loop-Guard-Token", "gateway token header: X-Loop-Guard-Token or Authorization")
	dedicated := flag.Bool("dedicated-connections", false, "use a separate HTTP/1.1 TCP/TLS connection per upstream request")
	flag.Parse()
	token := os.Getenv("LOOP_GUARD_TOKEN")
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatal("invalid listen address")
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && token == "" {
		log.Fatal("non-loopback listeners require LOOP_GUARD_TOKEN in the configured auth header")
	}
	gateway, err := loopguard.New(loopguard.Config{Upstream: *upstream, Token: token, TokenHeader: *authHeader, DedicatedConnections: *dedicated, OnTrip: func(d loopguard.Detection) {
		log.Printf("model_output_loop channel=%s window=%d distinct=%d", d.Channel, d.Window, d.Distinct)
	}})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: gateway, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}()
	log.Printf("loop guard listening on %s dedicated_connections=%t", *listen, *dedicated)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}
