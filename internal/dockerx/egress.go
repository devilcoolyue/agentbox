package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"agentbox/internal/netaccess"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// ErrEgressUnsupported means the workspace's network mode cannot be locked
// (host networking or another container's namespace).
var ErrEgressUnsupported = errors.New("受限出网要求工作空间使用独立的 Docker 网络")

func egressName(cid string) string {
	if len(cid) > 12 {
		cid = cid[:12]
	}
	return "agentbox-egress-" + cid
}

// StartedAt identifies the current network namespace of a running workspace:
// Docker creates a fresh namespace (and so drops our rules) on every start.
func (m *Manager) StartedAt(ctx context.Context, containerID string) (string, error) {
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}
	if info.State == nil || !info.State.Running {
		return "", fmt.Errorf("workspace container is not running")
	}
	return info.State.StartedAt, nil
}

// LockEgress installs restricted-egress rules into the workspace's network
// namespace through a one-shot NET_ADMIN container (the network helper image)
// and returns the StartedAt of the namespace it locked. The rules outlive the
// one-shot container; the agent itself never gains NET_ADMIN. A workspace with
// network mode "none" has nothing to lock and returns its StartedAt unchanged.
func (m *Manager) LockEgress(ctx context.Context, containerID, image, v4, v6 string) (string, error) {
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}
	if info.State == nil || !info.State.Running || info.HostConfig == nil {
		return "", fmt.Errorf("workspace container is not running")
	}
	mode := info.HostConfig.NetworkMode
	if mode.IsNone() {
		return info.State.StartedAt, nil
	}
	if mode.IsHost() || mode.IsContainer() {
		return "", ErrEgressUnsupported
	}
	name := egressName(containerID)
	if old, e := m.cli.ContainerInspect(ctx, name); e == nil {
		if old.Config == nil || old.Config.Labels["agentbox.egress"] == "" {
			return "", fmt.Errorf("egress helper name conflict")
		}
		if e = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); e != nil && !client.IsErrNotFound(e) {
			return "", e
		}
	} else if !client.IsErrNotFound(e) {
		return "", e
	}
	pids := int64(32)
	cc := &container.Config{
		Image: image, User: "0:0",
		Entrypoint: []string{"/bin/sh", "-c", netaccess.EgressScript}, Cmd: []string{},
		Env:         []string{"ABX_EGRESS_V4=" + v4, "ABX_EGRESS_V6=" + v6},
		Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		Labels:      map[string]string{"agentbox.egress": containerID},
	}
	hc := &container.HostConfig{
		NetworkMode: container.NetworkMode("container:" + containerID),
		CapDrop:     []string{"ALL"}, CapAdd: []string{"NET_ADMIN", "NET_RAW"},
		SecurityOpt: []string{"no-new-privileges:true"}, ReadonlyRootfs: true,
		Tmpfs:     map[string]string{"/run": "rw,noexec,nosuid,size=1m"},
		Resources: container.Resources{Memory: 64 << 20, NanoCPUs: 500000000, PidsLimit: &pids},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m", "max-file": "1"}},
	}
	created, err := m.cli.ContainerCreate(ctx, cc, hc, nil, nil, name)
	if err != nil {
		if client.IsErrNotFound(err) {
			return "", fmt.Errorf("受限出网需要网络辅助镜像 %s，请先构建: %w", image, err)
		}
		return "", fmt.Errorf("create egress helper: %w", err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = m.cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{Force: true})
	}()
	if err = m.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start egress helper: %w", err)
	}
	logs, err := m.cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return "", fmt.Errorf("egress helper logs: %w", err)
	}
	defer logs.Close()
	var out strings.Builder
	_, _ = stdcopy.StdCopy(io.Discard, &limitedWriter{w: &out, n: 4096}, logs)
	wait, waitErr := m.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case st := <-wait:
		if st.StatusCode != 0 {
			return "", fmt.Errorf("受限出网规则安装失败 (exit %d): %s", st.StatusCode, strings.TrimSpace(out.String()))
		}
	case err = <-waitErr:
		return "", fmt.Errorf("egress helper wait: %w", err)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return info.State.StartedAt, nil
}

// limitedWriter keeps the first n bytes and silently drops the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
