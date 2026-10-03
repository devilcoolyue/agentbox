package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

const commandOutputLimit = 4 << 20

type CommandError struct {
	Code   int
	Stderr string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("exec exited %d: %s", e.Code, e.Stderr)
}

func (e *CommandError) ExitCode() int { return e.Code }

// ExecCommand runs a short non-interactive command as the session user. It
// injects no account environment, captures bounded stdout/stderr, and aborts
// attached reads on cancellation. Callers must also bound the process inside
// the container: disconnecting an exec stream alone doesn't terminate it.
func (m *Manager) ExecCommand(ctx context.Context, containerID string, cmd []string) (string, error) {
	return m.ExecCommandEnv(ctx, containerID, cmd, nil)
}

// ExecCommandEnv is the bounded command transport for interactive-terminal
// preparation. Account values are passed through Docker's exec Env, never baked
// into the container or interpolated into a shell's command-line arguments.
func (m *Manager) ExecCommandEnv(ctx context.Context, containerID string, cmd []string, env []string) (string, error) {
	if env != nil {
		env = append([]string{"TERM=xterm-256color", "DISABLE_AUTOUPDATER=1"}, env...)
	}
	id, err := m.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User: execUser, WorkingDir: WorkspaceMount,
		Env:          env,
		AttachStdout: true, AttachStderr: true, Cmd: cmd,
	})
	if err != nil {
		return "", err
	}
	stream, err := m.cli.ContainerExecAttach(ctx, id.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, stream.Close)
	defer stop()

	var out, errOut bytes.Buffer
	stdout := &commandWriter{buf: &out, limit: commandOutputLimit}
	stderr := &commandWriter{buf: &errOut, limit: 8 << 10}
	_, readErr := stdcopy.StdCopy(stdout, stderr, stream.Reader)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if readErr != nil {
		return "", readErr
	}
	code, err := m.ExitCode(ctx, id.ID)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", &CommandError{Code: code, Stderr: strings.TrimSpace(errOut.String())}
	}
	if stdout.overflow {
		return "", errors.New("Git 输出超过 4 MiB，请在终端查看或缩小文件范围")
	}
	return out.String(), nil
}

// Keep draining oversized output so the child cannot block on a full pipe.
// Unlike capWriter, remember truncation so callers never treat partial Git
// status/diffs as complete results.
type commandWriter struct {
	buf      *bytes.Buffer
	limit    int
	overflow bool
}

func (w *commandWriter) Write(p []byte) (int, error) {
	n := len(p)
	left := w.limit - w.buf.Len()
	if len(p) > left {
		w.overflow = true
		p = p[:left]
	}
	_, _ = w.buf.Write(p)
	return n, nil
}
