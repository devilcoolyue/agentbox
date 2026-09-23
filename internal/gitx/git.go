// Package gitx executes workspace Git commands through a container runtime.
// It deliberately has no host-process fallback: repository configuration can
// execute code, so even read-only Git commands belong inside the session.
package gitx

import (
	"context"
	"errors"
	"path"
	"strings"
)

var ErrUnavailable = errors.New("Git 容器执行服务不可用")

// IsExit distinguishes an expected Git status from runtime/transport failures.
func IsExit(err error, code int) bool {
	var exited interface{ ExitCode() int }
	return errors.As(err, &exited) && exited.ExitCode() == code
}

// Executor runs argv as the container's unprivileged user, with bounded output.
type Executor interface {
	ExecCommand(context.Context, string, []string) (string, error)
}

// Prepare starts a session if necessary and holds it against idle reclamation.
// On success release must be non-nil. It must enforce access to code execution.
type Prepare func(context.Context, string) (containerID string, release func(), err error)

type Runner struct {
	exec    Executor
	prepare Prepare
}

func New(exec Executor, prepare Prepare) *Runner {
	return &Runner{exec: exec, prepare: prepare}
}

func (r *Runner) Run(ctx context.Context, sessionID, repo string, args ...string) (string, error) {
	if r == nil || r.exec == nil || r.prepare == nil {
		return "", ErrUnavailable
	}
	cmd, err := command(repo, args)
	if err != nil {
		return "", err
	}
	containerID, release, err := r.prepare(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if release == nil {
		return "", ErrUnavailable
	}
	defer release()
	if containerID == "" {
		return "", ErrUnavailable
	}
	return r.exec.ExecCommand(ctx, containerID, cmd)
}

func command(repo string, args []string) ([]string, error) {
	// Paths are relative Linux paths, independently of the server's OS. Reject
	// traversal before cleaning, and keep user strings in argv (never a shell).
	if path.IsAbs(repo) || strings.ContainsAny(repo, "\\\x00") {
		return nil, errors.New("invalid Git repository path")
	}
	for _, part := range strings.Split(repo, "/") {
		if part == ".." {
			return nil, errors.New("invalid Git repository path")
		}
	}
	dir := path.Join("/workspace", repo)
	// Closing a Docker exec connection doesn't kill its process. timeout runs
	// inside the container and bounds Git and its process group independently
	// of the request connection. Both utilities ship in the Debian agent image.
	cmd := []string{
		"/usr/bin/timeout", "--signal=TERM", "--kill-after=2s", "15s",
		"/usr/bin/env", "-i",
		"PATH=/usr/bin:/bin", "HOME=/home/agent", "LANG=C.UTF-8",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_LITERAL_PATHSPECS=1",
		"GIT_CEILING_DIRECTORIES=" + path.Dir(dir),
		"/usr/bin/git",
		"-c", "core.excludesFile=/dev/null",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false",
		"-c", "maintenance.auto=false",
		"-c", "gc.auto=0",
		"-C", dir,
	}
	return append(cmd, args...), nil
}
