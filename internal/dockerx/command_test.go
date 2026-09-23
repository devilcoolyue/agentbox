package dockerx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

func commandTestManager(t *testing.T, stdout, stderr string, code int, attached chan<- struct{}) *Manager {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/box/exec"):
			var opts container.ExecOptions
			if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if opts.User != "1000:1000" || opts.WorkingDir != "/workspace" || opts.Tty || opts.AttachStdin || !opts.AttachStdout || !opts.AttachStderr || len(opts.Env) != 0 {
				t.Errorf("unsafe exec options: %+v", opts)
			}
			if !slices.Equal(opts.Cmd, []string{"git-command", "argument with spaces"}) {
				t.Errorf("argv changed: %q", opts.Cmd)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"exec-1"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/exec-1/start"):
			// Drain the HTTP start body before hijacking; closing a TCP socket
			// with unread request bytes can reset it and discard our final output.
			_, _ = io.Copy(io.Discard, r.Body)
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = rw.Flush()
			if attached != nil {
				attached <- struct{}{}
				_, _ = io.Copy(io.Discard, conn) // exits when cancellation closes the stream
				return
			}
			_, _ = io.WriteString(stdcopy.NewStdWriter(conn, stdcopy.Stdout), stdout)
			_, _ = io.WriteString(stdcopy.NewStdWriter(conn, stdcopy.Stderr), stderr)
		case strings.HasSuffix(r.URL.Path, "/exec/exec-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, code)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &Manager{cli: cli}
}

func TestExecCommandCapturesOutputAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr string
		code                 int
		wantError            bool
	}{
		{"success", "diff output", "warning", 0, false},
		{"failure", "partial output", "repository denied", 128, true},
		{"overflow", strings.Repeat("x", commandOutputLimit+1), "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := commandTestManager(t, tc.stdout, tc.stderr, tc.code, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			out, err := m.ExecCommand(ctx, "box", []string{"git-command", "argument with spaces"})
			if (err != nil) != tc.wantError {
				t.Fatalf("out length=%d error=%v", len(out), err)
			}
			if !tc.wantError && out != tc.stdout {
				t.Fatalf("stdout=%q", out)
			}
			if tc.wantError && out != "" {
				t.Fatal("returned incomplete output as usable data")
			}
			if tc.code != 0 {
				var exited *CommandError
				if !errors.As(err, &exited) || exited.Code != tc.code || !strings.Contains(err.Error(), tc.stderr) {
					t.Fatalf("exit code/stderr lost: %v", err)
				}
			}
		})
	}
}

func TestExecCommandCancellationUnblocksAttachedRead(t *testing.T) {
	attached := make(chan struct{}, 1)
	m := commandTestManager(t, "", "", 0, attached)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.ExecCommand(ctx, "box", []string{"git-command", "argument with spaces"})
		done <- err
	}()
	select {
	case <-attached:
	case <-time.After(5 * time.Second):
		t.Fatal("exec never attached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation left the Docker stream blocked")
	}
}
