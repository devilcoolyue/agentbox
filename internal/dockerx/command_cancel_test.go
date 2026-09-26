package dockerx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitSupervisorCancelsProcessGroup(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// Ignore TERM to exercise the bounded KILL path. No credentials or Docker.
	script := "import signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); print('ready',flush=True); time.sleep(30)"
	cmd := exec.CommandContext(ctx, python, "-I", "-c", gitSupervisor, python, "-I", "-c", script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("child not ready: %q %v", line, err)
	}
	started := time.Now()
	_ = stdin.Close()
	err = cmd.Wait()
	var code int
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	}
	if code != 130 || time.Since(started) > 4*time.Second || ctx.Err() != nil {
		t.Fatalf("cancellation code=%d elapsed=%s err=%v", code, time.Since(started), err)
	}
}
func TestGitSupervisorPreservesArgumentsAndExit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	message := "space ' quote $()\nsecond line"
	cmd := exec.CommandContext(ctx, python, "-I", "-c", gitSupervisor, python, "-I", "-c", "import sys; print(sys.argv[1]); sys.exit(7)", message)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	out, err := cmd.CombinedOutput()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 7 || string(out) != message+"\n" {
		t.Fatalf("argv/exit changed: %q %v", out, err)
	}
}

func TestCancelableExecSignalsEOFAndWaitsForExit(t *testing.T) {
	attached := make(chan struct{})
	eof := make(chan struct{})
	allowExit := make(chan struct{})
	var inspected atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/box/exec"):
			var opts container.ExecOptions
			if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
				t.Error(err)
				return
			}
			if !opts.AttachStdin || opts.User != "1000:1000" || len(opts.Env) != 0 || opts.Cmd[0] != "/usr/bin/python3" || opts.Cmd[1] != "-I" || opts.Cmd[3] != gitSupervisor || opts.Cmd[len(opts.Cmd)-1] != "literal argument" {
				t.Errorf("unsafe supervisor options: %+v", opts)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"Id":"supervised"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/supervised/start"):
			io.Copy(io.Discard, r.Body)
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			io.WriteString(rw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			rw.Flush()
			close(attached)
			io.Copy(io.Discard, conn)
			close(eof)
			<-allowExit
		case strings.HasSuffix(r.URL.Path, "/exec/supervised/json"):
			inspected.Store(true)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"Running":false,"ExitCode":130}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	manager := &Manager{cli: cli}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.ExecCancelableCommand(ctx, "box", []string{"/usr/bin/git", "literal argument"})
		done <- err
	}()
	select {
	case <-attached:
	case <-time.After(3 * time.Second):
		close(allowExit)
		t.Fatal("not attached")
	}
	cancel()
	select {
	case <-eof:
	case <-time.After(3 * time.Second):
		close(allowExit)
		t.Fatal("cancel did not send EOF")
	}
	select {
	case err := <-done:
		close(allowExit)
		t.Fatalf("returned before supervisor exit: %v", err)
	default:
	}
	close(allowExit)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !inspected.Load() {
			t.Fatalf("exit: %v inspected=%v", err, inspected.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not return after exit")
	}
}
