package dockerx

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

//go:embed git_supervisor.py
var gitSupervisor string

// ExecCancelableCommand keeps the attachment alive briefly after cancellation,
// using stdin EOF to stop the command's process group and collect its exit.
// Python is already part of the Agent image; no helper is written into a mount.
func (m *Manager) ExecCancelableCommand(ctx context.Context, containerID string, cmd []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	argv := append([]string{"/usr/bin/python3", "-I", "-c", gitSupervisor}, cmd...)
	id, err := m.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{User: execUser, WorkingDir: WorkspaceMount, AttachStdin: true, AttachStdout: true, AttachStderr: true, Cmd: argv})
	if err != nil {
		return "", err
	}
	stream, err := m.cli.ContainerExecAttach(ctx, id.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var mu sync.Mutex
	var force *time.Timer
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		mu.Lock()
		defer mu.Unlock()
		_ = stream.CloseWrite()
		force = time.AfterFunc(5*time.Second, stream.Close)
	})
	defer func() {
		if !stop() {
			<-cancelDone
		}
		mu.Lock()
		defer mu.Unlock()
		if force != nil {
			force.Stop()
		}
	}()
	var out, errOut bytes.Buffer
	stdout := &commandWriter{buf: &out, limit: commandOutputLimit}
	stderr := &commandWriter{buf: &errOut, limit: 8 << 10}
	var errWriter io.Writer = stderr
	var progress *progressWriter
	if fn := stderrLines(ctx); fn != nil {
		progress = &progressWriter{next: stderr, fn: fn}
		errWriter = progress
	}
	_, readErr := stdcopy.StdCopy(stdout, errWriter, stream.Reader)
	if progress != nil {
		progress.flush()
	}
	inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	code, exitErr := m.ExitCode(inspectCtx, id.ID)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if readErr != nil {
		return "", readErr
	}
	if exitErr != nil {
		return "", exitErr
	}
	if code != 0 {
		return "", &CommandError{Code: code, Stderr: strings.TrimSpace(errOut.String())}
	}
	if stdout.overflow {
		return "", errors.New("Git 输出超过 4 MiB，请缩小操作范围")
	}
	return out.String(), nil
}
