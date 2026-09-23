package dockerx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

func TestExecConnectionsCloseOnCancellation(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "pty"}[pty], func(t *testing.T) {
			peerClosed := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/containers/box/exec") {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"Id":"id"}`)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				defer close(peerClosed)
				rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
				rw.Flush()
				io.Copy(io.Discard, conn)
			}))
			defer srv.Close()
			cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.45"))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			m := &Manager{cli: cli}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			readDone := make(chan struct{})
			if pty {
				stream, err := m.ExecPTY(ctx, "box", []string{"sleep", "infinity"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				go func() { defer close(readDone); io.Copy(io.Discard, stream.Reader) }()
			} else {
				stream, err := m.ExecStream(ctx, "box", []string{"sleep", "infinity"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				go func() { defer close(readDone); stream.Demux(io.Discard, io.Discard) }()
			}
			cancel()
			for _, done := range []chan struct{}{readDone, peerClosed} {
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("Docker hijack survived cancellation")
				}
			}
		})
	}
}
