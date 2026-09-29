// Package dockerx wraps the Docker Engine API for agentbox: one container per
// active session, plus exec-based PTY and streaming channels into it.
package dockerx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

const (
	// AgentUID / AgentGID must match the "agent" user baked into the image.
	AgentUID = 1000
	AgentGID = 1000

	ContainerHome  = "/home/agent"
	WorkspaceMount = "/workspace"
	SharedMount    = "/shared" // 同一用户所有会话共用的目录
	execUser       = "1000:1000"
)

type Manager struct {
	cli *client.Client
	cfg *config.Config
}

func New(cfg *config.Config) (*Manager, error) { return NewContext(context.Background(), cfg) }

func NewContext(parent context.Context, cfg *config.Config) (*Manager, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		return nil, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	return &Manager{cli: cli, cfg: cfg}, nil
}

func (m *Manager) Close() error { return m.cli.Close() }

// NetworkGateway returns the gateway IP of a docker network (the address a
// container reaches the host at). Used to sanity-check the tunnel proxy bind.
func (m *Manager) NetworkGateway(ctx context.Context, name string) (string, error) {
	n, err := m.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		return "", err
	}
	for _, cfg := range n.IPAM.Config {
		if cfg.Gateway != "" {
			return cfg.Gateway, nil
		}
	}
	return "", fmt.Errorf("network %q has no gateway configured", name)
}

// ContainerIPs returns the container's IP on every network it is attached to
// (empty when stopped or unknown). Tunnel port-map listeners authorize
// connections by these source addresses.
func (m *Manager) ContainerIPs(ctx context.Context, containerID string) []string {
	if containerID == "" {
		return nil
	}
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil || info.NetworkSettings == nil {
		return nil
	}
	var out []string
	for _, nw := range info.NetworkSettings.Networks {
		if nw.IPAddress != "" {
			out = append(out, nw.IPAddress)
		}
	}
	return out
}

// ServerVersion returns the Docker daemon version, or "" when unreachable.
func (m *Manager) ServerVersion(ctx context.Context) string {
	v, err := m.cli.ServerVersion(ctx)
	if err != nil {
		return ""
	}
	return v.Version
}

// ContainerState reports whether the container is running and when it last
// started (zero time when stopped, removed or unknown). The运维监控 uses it to
// show per-session uptime without pulling full stats for stopped containers.
func (m *Manager) ContainerState(ctx context.Context, containerID string) (bool, time.Time) {
	if containerID == "" {
		return false, time.Time{}
	}
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil || info.State == nil {
		return false, time.Time{}
	}
	started, _ := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	return info.State.Running, started
}

// RawStat is a cumulative resource snapshot of one container. CPU is a
// monotonically increasing counter, so deriving a percentage needs two
// snapshots a known window apart (see StatSnapshots).
type RawStat struct {
	CPUTotal uint64    // cpu_usage.total_usage, nanoseconds of CPU time
	MemUsage uint64    // resident memory minus reclaimable file cache, bytes
	MemLimit uint64    // container memory limit, bytes (host total when unlimited)
	Pids     uint64    // processes currently in the cgroup
	Read     time.Time // daemon-side read timestamp, the window's true clock
	OK       bool      // false when the snapshot could not be taken
}

// statSnapshot reads one-shot stats for a single container. OneShot omits the
// daemon's own precpu frame, so callers pair two of these to get a rate.
func (m *Manager) statSnapshot(ctx context.Context, containerID string) RawStat {
	resp, err := m.cli.ContainerStatsOneShot(ctx, containerID)
	if err != nil {
		return RawStat{}
	}
	defer resp.Body.Close()
	var s container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return RawStat{}
	}
	return RawStat{
		CPUTotal: s.CPUStats.CPUUsage.TotalUsage,
		MemUsage: memUsage(s.MemoryStats),
		MemLimit: s.MemoryStats.Limit,
		Pids:     s.PidsStats.Current,
		Read:     s.Read,
		OK:       true,
	}
}

// memUsage mirrors `docker stats`: resident usage minus the reclaimable file
// cache (inactive_file on cgroup v2, total_inactive_file on cgroup v1).
func memUsage(m container.MemoryStats) uint64 {
	cache := m.Stats["inactive_file"]
	if cache == 0 {
		cache = m.Stats["total_inactive_file"]
	}
	if cache > m.Usage {
		return m.Usage
	}
	return m.Usage - cache
}

// StatSnapshots samples every id concurrently. Call it twice a short window
// apart and diff the CPU counters to turn them into percentages; a container
// that fails to sample yields a !OK entry the caller skips.
func (m *Manager) StatSnapshots(ctx context.Context, ids []string) map[string]RawStat {
	out := make(map[string]RawStat, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			st := m.statSnapshot(ctx, id)
			mu.Lock()
			out[id] = st
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}

// IsRunning reports whether the container exists and is running.
func (m *Manager) IsRunning(ctx context.Context, containerID string) bool {
	if containerID == "" {
		return false
	}
	info, err := m.cli.ContainerInspect(ctx, containerID)
	return err == nil && info.State != nil && info.State.Running
}

// RunningWithMount reports whether the container is running AND has dest
// mounted. Containers from before a mount was introduced fail this check so
// the start path falls through to EnsureRunning, which recreates them.
func (m *Manager) RunningWithMount(ctx context.Context, containerID, dest string) bool {
	if containerID == "" {
		return false
	}
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil || info.State == nil || !info.State.Running {
		return false
	}
	for _, mnt := range info.Mounts {
		if mnt.Destination == dest {
			return true
		}
	}
	return false
}

// baseContainerEnv 是每个会话容器创建时写死的那几个变量。
//
// 账号 env（中转站的 ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN 等）**不烘进
// 容器**：容器 env 在 create 那一刻定死，之后只能靠 exec 往上加，减不掉。账号
// 从中转站切回订阅登录时 clearClaudeRelay 会把令牌从 config.json 里摘掉，但旧
// 容器里那份还在，而 claude CLI 认 env 里的 Bearer 令牌优先于 OAuth 凭证——订阅
// 登录形同虚设，CLI 卡在重试里直到被杀（回合报「进程退出码 137」，stderr 只留下
// 一句 connectors are disabled 的告警）。所以账号 env 一律走每次 exec 注入
// （server.execEnv），加和减都立刻生效。
func baseContainerEnv() []string {
	// CLI 属 root、容器跑 agent 用户，自升级必然失败；镜像 ENV 也设了，
	// 这里再设一份是让旧镜像建出的容器同样安静。
	return []string{"HOME=" + ContainerHome, "TERM=xterm-256color", "LANG=C.UTF-8", "DISABLE_AUTOUPDATER=1"}
}

// hasBakedEnv 判断容器创建时有没有被写进 base/镜像之外的变量——那是旧版本把
// 账号 env 烘进去的痕迹，这种容器留着就永远摘不掉密钥，只能重建。
// 只比键不比值：镜像里 NODE_VERSION 那类值本来就会随镜像更新而变。
func hasBakedEnv(containerEnv, imageEnv []string) bool {
	known := map[string]bool{}
	for _, kv := range baseContainerEnv() {
		known[envKey(kv)] = true
	}
	for _, kv := range imageEnv {
		known[envKey(kv)] = true
	}
	for _, kv := range containerEnv {
		if !known[envKey(kv)] {
			return true
		}
	}
	return false
}

func envKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

// EnsureRunning brings the session's container up, reusing an existing one
// when possible, and returns its id.
func (m *Manager) EnsureRunning(ctx context.Context, sess store.Session, acct config.Account, workspaceDir, homeDir, sharedDir string) (string, error) {
	if sess.ContainerID != "" {
		info, err := m.cli.ContainerInspect(ctx, sess.ContainerID)
		if client.IsErrNotFound(err) {
			if err := m.removeNetwork(ctx, sess.ContainerID); err != nil {
				return "", err
			}
		}
		if err == nil {
			hasShared := false
			for _, mnt := range info.Mounts {
				if mnt.Destination == SharedMount {
					hasShared = true
					break
				}
			}
			// 镜像 tag 被重建后，旧容器仍指向旧镜像层；停着的容器直接换新。
			imageFresh := true
			// 老版本把账号 env 烘进了容器，那样的容器摘不掉密钥，只能重建。
			envFresh := true
			if img, ierr := m.cli.ImageInspect(ctx, m.cfg.GetAgentImage()); ierr == nil {
				imageFresh = info.Image == img.ID
				if info.Config != nil {
					envFresh = !hasBakedEnv(info.Config.Env, img.Config.Env)
				}
			}
			if hasShared && envFresh {
				if info.State != nil && info.State.Running {
					return sess.ContainerID, nil // 运行中不打断，停一次后吃到新镜像
				}
				if imageFresh {
					if err := m.cli.ContainerStart(ctx, sess.ContainerID, container.StartOptions{}); err == nil {
						return sess.ContainerID, nil
					}
				}
			}
			// Unstartable leftover, a pre-/shared container, or a stale image:
			// replace it (workspace/home live on the host, so recreation loses
			// nothing).
			if err := m.removeNetwork(ctx, sess.ContainerID); err != nil {
				return "", err
			}
			_ = m.cli.ContainerRemove(ctx, sess.ContainerID, container.RemoveOptions{Force: true})
		}
	}

	env := baseContainerEnv()
	lim := m.cfg.GetContainer()
	pids := lim.PidsLimit
	initProc := true // run an init (tini) as PID 1 to reap orphaned zombies
	cc := &container.Config{
		Image:      m.cfg.GetAgentImage(),
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: WorkspaceMount,
		User:       execUser,
		Env:        env,
		Labels: map[string]string{
			"agentbox.session": sess.ID,
			"agentbox.user":    sess.User,
			"agentbox.account": acct.ID,
		},
	}
	hc := &container.HostConfig{
		Binds: []string{
			workspaceDir + ":" + WorkspaceMount,
			homeDir + ":" + ContainerHome,
			sharedDir + ":" + SharedMount,
		},
		NetworkMode: container.NetworkMode(lim.Network),
		SecurityOpt: []string{"no-new-privileges:true"},
		// PID 1 is `sleep infinity`, which never reaps children; without an init
		// the zombies from every exec'd process tree would slowly exhaust
		// PidsLimit on these long-lived containers.
		Init: &initProc,
		Resources: container.Resources{
			Memory:    lim.MemoryMB << 20,
			NanoCPUs:  int64(lim.CPUs * 1e9),
			PidsLimit: &pids,
		},
	}

	if img, err := m.cli.ImageInspect(ctx, m.cfg.GetAgentImage()); err == nil {
		browserContainerOptions(cc, hc, img.Config.Labels)
	}

	name := "agentbox-" + sess.ID
	resp, err := m.cli.ContainerCreate(ctx, cc, hc, nil, nil, name)
	if err != nil {
		if strings.Contains(err.Error(), "is already in use") {
			// Stale container from a lost state file: remove by name and retry.
			_ = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
			resp, err = m.cli.ContainerCreate(ctx, cc, hc, nil, nil, name)
		}
		if err != nil {
			return "", fmt.Errorf("create container: %w", err)
		}
	}
	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	return resp.ID, nil
}

func (m *Manager) Stop(ctx context.Context, containerID string) error {
	if err := m.closeBrowser(ctx, containerID); err != nil {
		return err
	}
	timeout := 10
	err := m.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	if err == nil || client.IsErrNotFound(err) {
		return m.removeNetwork(ctx, containerID)
	}
	return err
}

func (m *Manager) Remove(ctx context.Context, containerID string) error {
	if err := m.closeBrowser(ctx, containerID); err != nil {
		return err
	}
	if err := m.removeNetwork(ctx, containerID); err != nil {
		return err
	}
	err := m.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if client.IsErrNotFound(err) {
		return nil
	}
	return err
}

// PTY is an attached interactive exec (Tty=true).
type PTY struct {
	ExecID string
	Conn   net.Conn
	Reader *bufio.Reader
	closer func()
}

func (p *PTY) Close() { p.closer() }

// extraEnv carries per-account variables (e.g. ANTHROPIC_BASE_URL): container
// env is frozen at creation, so exec-time injection is what makes account env
// edits reach containers that already exist.
func (m *Manager) ExecPTY(ctx context.Context, containerID string, cmd []string, extraEnv []string) (*PTY, error) {
	idResp, err := m.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User:         execUser,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   WorkspaceMount,
		// DISABLE_AUTOUPDATER also lives in the image ENV and container env,
		// but containers created from older images have neither; exec env is
		// the only knob that reaches those without recreating them.
		Env: append([]string{"TERM=xterm-256color", "DISABLE_AUTOUPDATER=1"}, extraEnv...),
		Cmd: cmd,
	})
	if err != nil {
		return nil, err
	}
	hj, err := m.cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, err
	}
	return &PTY{ExecID: idResp.ID, Conn: hj.Conn, Reader: hj.Reader, closer: closeOnCancel(ctx, hj.Close)}, nil
}

func (m *Manager) ResizePTY(ctx context.Context, execID string, cols, rows uint) error {
	return m.cli.ContainerExecResize(ctx, execID, container.ResizeOptions{Width: cols, Height: rows})
}

// Stream is a non-TTY exec used for headless chat turns: stdin carries the
// prompt, stdout carries JSONL events.
type Stream struct {
	ExecID string
	conn   net.Conn
	reader *bufio.Reader
	closer func()
}

func (m *Manager) ExecStream(ctx context.Context, containerID string, cmd []string, extraEnv []string) (*Stream, error) {
	idResp, err := m.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User:         execUser,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   WorkspaceMount,
		Env:          append([]string{"DISABLE_AUTOUPDATER=1"}, extraEnv...),
		Cmd:          cmd,
	})
	if err != nil {
		return nil, err
	}
	hj, err := m.cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	return &Stream{ExecID: idResp.ID, conn: hj.Conn, reader: hj.Reader, closer: closeOnCancel(ctx, hj.Close)}, nil
}

func (s *Stream) Write(p []byte) (int, error) { return s.conn.Write(p) }

// CloseWrite signals EOF on the agent's stdin.
func (s *Stream) CloseWrite() error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := s.conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return s.conn.Close()
}

// Demux copies the multiplexed exec output into stdout/stderr writers,
// returning when the process exits or the connection drops.
func (s *Stream) Demux(stdout, stderr interface{ Write([]byte) (int, error) }) error {
	_, err := stdcopy.StdCopy(stdout, stderr, s.reader)
	return err
}

func (s *Stream) Close() { s.closer() }

// ExitCode waits briefly for the exec to settle and returns its exit code.
func (m *Manager) ExitCode(ctx context.Context, execID string) (int, error) {
	for i := 0; i < 50; i++ {
		insp, err := m.cli.ContainerExecInspect(ctx, execID)
		if err != nil {
			return -1, err
		}
		if !insp.Running {
			return insp.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return -1, fmt.Errorf("exec still running")
}

// ExecCapture runs cmd to completion, feeding stdin, and returns its stdout as
// a string (stderr discarded). Meant for short, bounded helper calls such as
// thread-title generation — not for long agent turns. Output is capped so a
// misbehaving command can't balloon memory.
func (m *Manager) ExecCapture(ctx context.Context, containerID string, cmd, extraEnv []string, stdin string) (string, error) {
	stream, err := m.ExecStream(ctx, containerID, cmd, extraEnv)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	if stdin != "" {
		if _, err := stream.Write([]byte(stdin)); err != nil {
			return "", err
		}
	}
	if err := stream.CloseWrite(); err != nil {
		return "", err
	}
	var out bytes.Buffer
	capped := &capWriter{w: &out, left: 64 << 10}
	if err := stream.Demux(capped, io.Discard); err != nil {
		return "", err
	}
	code, err := m.ExitCode(ctx, stream.ExecID)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exec exited %d", code)
	}
	return out.String(), nil
}

// capWriter forwards at most left bytes to w, silently dropping the rest.
type capWriter struct {
	w    io.Writer
	left int
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.left <= 0 {
		return len(p), nil
	}
	if len(p) > c.left {
		if _, err := c.w.Write(p[:c.left]); err != nil {
			return 0, err
		}
		c.left = 0
		return len(p), nil
	}
	c.left -= len(p)
	return c.w.Write(p)
}

// ExecFireAndForget runs a short command (e.g. interrupt) detached.
func (m *Manager) ExecFireAndForget(ctx context.Context, containerID string, cmd []string) error {
	idResp, err := m.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User:   execUser,
		Detach: true,
		Cmd:    cmd,
	})
	if err != nil {
		return err
	}
	return m.cli.ContainerExecStart(ctx, idResp.ID, container.ExecStartOptions{Detach: true})
}

// Docker hijacks outlive HTTP request cancellation. Close the stream explicitly
// so blocked PTY reads, stdin writes and demux calls can finish on shutdown.
func closeOnCancel(ctx context.Context, close func()) func() {
	var once sync.Once
	closeOnce := func() { once.Do(close) }
	stop := context.AfterFunc(ctx, closeOnce)
	return func() { stop(); closeOnce() }
}

// Running distinguishes daemon failures from a missing or stopped container.
func (m *Manager) Running(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	info, err := m.cli.ContainerInspect(ctx, id)
	if client.IsErrNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.State != nil && info.State.Running, nil
}
