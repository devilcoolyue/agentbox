package dockerx

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agentbox/internal/gitx"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Opt-in: uses only a throwaway container, without host mounts or real
// credentials. Build the fixture described in docs/development.md first.
func TestGitContainerLive(t *testing.T) {
	image := os.Getenv("AGENTBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set AGENTBOX_DOCKER_TEST_IMAGE to run the Linux container smoke test")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image: image, User: execUser, WorkingDir: WorkspaceMount,
		Cmd: []string{"sleep", "infinity"},
		Env: []string{"AGENTBOX_TEST_SECRET=must-not-reach-git"},
	}, &container.HostConfig{
		NetworkMode: "none", SecurityOpt: []string{"no-new-privileges:true"},
		Resources: container.Resources{Memory: 128 << 20},
	}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{Force: true}); err != nil {
			t.Errorf("remove test container: %v", err)
		}
	})
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cli: cli}
	uid, err := m.ExecCommand(ctx, created.ID, []string{"/usr/bin/id", "-u"})
	if err != nil || strings.TrimSpace(uid) != "1000" {
		t.Fatalf("user=%q err=%v", uid, err)
	}
	fixture := `set -eu
mkdir -p /workspace/project
cd /workspace/project
git init -q
printf 'hello\n' > file.txt
printf '*.txt filter=check\n' > .gitattributes
git config filter.check.clean 'env > /workspace/git-environment; cat'
printf '#!/bin/sh\ntouch /workspace/hook-ran\nexit 1\n' > .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit
`
	if _, err := m.ExecCommand(ctx, created.ID, []string{"/bin/sh", "-c", fixture}); err != nil {
		t.Fatal(err)
	}
	runner := gitx.New(m, func(context.Context, string) (string, func(), error) {
		return created.ID, func() {}, nil
	})
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=test", "-c", "user.email=test@localhost", "commit", "-m", "first"},
	} {
		if _, err := runner.Run(ctx, "fixture", "project", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	check := `test ! -e /workspace/hook-ran && cat /workspace/git-environment`
	env, err := m.ExecCommand(ctx, created.ID, []string{"/bin/sh", "-c", check})
	if err != nil {
		t.Fatalf("hook/environment check: %v", err)
	}
	if strings.Contains(env, "AGENTBOX_TEST_SECRET") || strings.Contains(env, "must-not-reach-git") {
		t.Fatal("inherited secret reached Git filter")
	}
	if _, err := m.ExecCommand(ctx, created.ID, []string{"/bin/sh", "-c", "printf 'changed\\n' >> /workspace/project/file.txt"}); err != nil {
		t.Fatal(err)
	}
	diff, err := runner.Run(ctx, "fixture", "project", "diff", "--no-ext-diff", "--no-textconv", "HEAD", "--", "file.txt")
	if err != nil || !strings.Contains(diff, "+changed") {
		t.Fatalf("diff=%q err=%v", diff, err)
	}
	cancelCtx, cancelCommand := context.WithCancel(ctx)
	done := make(chan error, 1)
	script := "import os,signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); open('/workspace/cancel-pid','w').write(str(os.getpid())); time.sleep(60)"
	go func() {
		_, err := m.ExecCancelableCommand(cancelCtx, created.ID, []string{"/usr/bin/python3", "-I", "-c", script})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if _, err = m.ExecCommand(ctx, created.ID, []string{"/usr/bin/test", "-f", "/workspace/cancel-pid"}); err == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			cancelCommand()
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !ready {
		cancelCommand()
		t.Fatal("supervised process did not start")
	}
	cancelCommand()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cancel did not finish")
	}
	if _, err = m.ExecCommand(ctx, created.ID, []string{"/usr/bin/python3", "-I", "-c", "import os; pid=int(open('/workspace/cancel-pid').read()); assert not os.path.exists('/proc/'+str(pid))"}); err != nil {
		t.Fatalf("cancelled child still alive: %v", err)
	}

}
