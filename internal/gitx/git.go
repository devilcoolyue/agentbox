// Package gitx executes workspace Git commands through a container runtime.
// It deliberately has no host-process fallback: repository configuration can
// execute code, so even read-only Git commands belong inside the session.
package gitx

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"
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
	mu      sync.Mutex
	locks   map[string]*repoLock
}

type repoLock struct {
	token chan struct{}
	refs  int
}

// Lock serializes compound web mutations. Git's own index.lock only covers a
// single command, not add+commit+read-SHA. CLI processes still use Git's locks.
func (r *Runner) Lock(ctx context.Context, sessionID, repo string) (func(), error) {
	if r == nil || r.exec == nil || r.prepare == nil {
		return nil, ErrUnavailable
	}
	if _, err := command(repo, nil); err != nil {
		return nil, err
	}
	key := sessionID + "\x00" + path.Clean(repo)
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[string]*repoLock{}
	}
	l := r.locks[key]
	if l == nil {
		l = &repoLock{token: make(chan struct{}, 1)}
		l.token <- struct{}{}
		r.locks[key] = l
	}
	l.refs++
	r.mu.Unlock()
	drop := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		l.refs--
		if l.refs == 0 {
			delete(r.locks, key)
		}
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-l.token:
		var once sync.Once
		return func() { once.Do(func() { l.token <- struct{}{}; drop() }) }, nil
	}
}

func New(exec Executor, prepare Prepare) *Runner {
	return &Runner{exec: exec, prepare: prepare}
}

func (r *Runner) Run(ctx context.Context, sessionID, repo string, args ...string) (string, error) {
	return r.run(ctx, sessionID, repo, false, args...)
}

// RunNetwork uses a longer in-container timeout for bounded network transfers.
// The URL must be an operation-scoped transport grant, never a provider token.
func (r *Runner) RunNetwork(ctx context.Context, sessionID, repo string, args ...string) (string, error) {
	return r.run(ctx, sessionID, repo, true, args...)
}

func (r *Runner) run(ctx context.Context, sessionID, repo string, network bool, args ...string) (string, error) {
	if r == nil || r.exec == nil || r.prepare == nil {
		return "", ErrUnavailable
	}
	cmd, err := command(repo, args)
	if err != nil {
		return "", err
	}
	if network {
		cmd[3] = "120s"
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
	if executor, ok := r.exec.(interface {
		ExecCancelableCommand(context.Context, string, []string) (string, error)
	}); ok {
		return executor.ExecCancelableCommand(ctx, containerID, cmd)
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
