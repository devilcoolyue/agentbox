package dockerx

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"agentbox/internal/collaboration"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
)

var ErrProjectCleanup = errors.New("project helper cleanup failed")

// WithProjectReader creates an offline, read-only Git helper with no login
// material. source is a service-owned workspace mount root, not an arbitrary
// repository subdirectory; repo selection happens inside this container.
func (m *Manager) WithProjectReader(ctx context.Context, image, source string, read func(string) error) (err error) {
	return m.withProjectContainer(ctx, image, source, true, "", read)
}

// InitializeCollaborationProject runs only on a freshly imported private tree.
// It creates independent Git history without importing source remotes/hooks.
func (m *Manager) InitializeCollaborationProject(ctx context.Context, image, source string) error {
	return m.withProjectContainer(ctx, image, source, false, "", func(cid string) error {
		for _, flag := range []string{"-e", "-L"} {
			if _, err := m.ExecCommand(ctx, cid, []string{"/usr/bin/test", "!", flag, "/workspace/.git"}); err != nil {
				return errors.New("task baseline repository already exists")
			}
		}
		base := []string{"/usr/bin/timeout", "--kill-after=2s", "60s", "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "HOME=/home/agent", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "/usr/bin/git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgSign=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0"}
		for _, args := range [][]string{{"init", "--quiet"}, {"add", "--all"}, {"-c", "user.name=Agentbox", "-c", "user.email=collaboration@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "Task baseline"}} {
			command := append(append([]string{}, base...), args...)
			if _, err := m.ExecCancelableCommand(ctx, cid, command); err != nil {
				return err
			}
		}
		return nil
	})
}

func CollaborationVerificationArgv(command string) []string {
	return []string{"/usr/bin/timeout", "--signal=TERM", "--kill-after=5s", "600s", "/usr/bin/env", "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/agent", "LANG=C.UTF-8", "/bin/sh", "-c", command}
}
func (m *Manager) RunCollaborationVerification(ctx context.Context, image, source, command, helperID string) (string, error) {
	result, err := m.VerifyCollaborationCommand(ctx, image, source, command, helperID)
	return result.Output, err
}
func (m *Manager) VerifyCollaborationCommand(ctx context.Context, image, source, command, helperID string) (CommandResult, error) {
	result := CommandResult{}
	if !collaboration.ValidID(helperID) || len(command) == 0 || len(command) > 8192 || strings.ContainsRune(command, 0) {
		return result, errors.New("invalid verification command")
	}
	err := m.withProjectContainer(ctx, image, source, false, helperID, func(cid string) error {
		var err error
		result, err = m.execCancelableResult(ctx, cid, CollaborationVerificationArgv(command))
		return err
	})
	return result, err
}

func (m *Manager) withProjectContainer(ctx context.Context, image, source string, readonly bool, helperID string, read func(string) error) (err error) {
	if !imageIDRE.MatchString(image) || !filepath.IsAbs(source) || filepath.Clean(source) != source || source == "/" || strings.ContainsAny(source, "\x00\r\n") {
		return errors.New("invalid project reader")
	}
	info, err := m.cli.ImageInspect(ctx, image)
	if err != nil {
		return err
	}
	if info.Config == nil || len(info.Config.Volumes) > 0 || len(info.Config.OnBuild) > 0 || info.Os != "linux" {
		return errors.New("invalid project reader image")
	}
	env := []string{}
	for _, v := range info.Config.Env {
		key, _, _ := strings.Cut(v, "=")
		env = append(env, key+"=")
	}
	env = append(env, "PATH=/usr/bin:/bin", "HOME=/home/agent", "LANG=C.UTF-8")
	pids := int64(64)
	init := true
	labels := map[string]string{"agentbox.collaboration-reader": "1"}
	name := ""
	if helperID != "" {
		if !collaboration.ValidID(helperID) {
			return errors.New("invalid project helper identity")
		}
		name = "agentbox-verify-" + helperID
		labels["agentbox.collaboration-verification"] = helperID
	}
	response, err := m.cli.ContainerCreate(ctx, &container.Config{Image: image, User: execUser, WorkingDir: WorkspaceMount, Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Env: env, NetworkDisabled: true, Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}, Labels: labels}, &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Init: &init, Resources: container.Resources{Memory: 512 << 20, NanoCPUs: 1_000_000_000, PidsLimit: &pids}, Mounts: []mount.Mount{{Type: mount.TypeBind, Source: source, Target: WorkspaceMount, ReadOnly: readonly}}, Tmpfs: map[string]string{ContainerHome: "rw,nosuid,nodev,size=8m,uid=1000,gid=1000", "/tmp": "rw,nosuid,nodev,size=16m,mode=1777"}}, nil, nil, name)
	if err != nil {
		// A lost create response may still leave a named container behind.
		// Recovery must inspect it before releasing the verification fence.
		if helperID != "" {
			return errors.Join(err, ErrProjectCleanup)
		}
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := m.cli.ContainerRemove(cleanup, response.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); cleanupErr != nil {
			err = errors.Join(err, ErrProjectCleanup, cleanupErr)
		}
	}()
	if err = m.cli.ContainerStart(ctx, response.ID, container.StartOptions{}); err != nil {
		return err
	}
	return read(response.ID)
}

// RemoveCollaborationVerification resolves a persisted helper identity after
// crash/cleanup failure. Never remove a foreign name collision; remove by the
// inspected immutable container ID to avoid a name replacement race.
func (m *Manager) RemoveCollaborationVerification(ctx context.Context, helperID string) error {
	if !collaboration.ValidID(helperID) {
		return errors.New("invalid verification identity")
	}
	info, err := m.cli.ContainerInspect(ctx, "agentbox-verify-"+helperID)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.ID == "" || info.Config == nil || info.Config.Labels["agentbox.collaboration-verification"] != helperID || info.Config.Labels["agentbox.collaboration-reader"] != "1" {
		return errors.New("verification helper ownership mismatch")
	}
	err = m.cli.ContainerRemove(ctx, info.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (m *Manager) RunningCollaborationHelpers(ctx context.Context) (int, error) {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, box := range containers {
		if box.Labels["agentbox.collaboration-reader"] == "1" || box.Labels["agentbox.collaboration-probe"] == "1" {
			n++
		}
	}
	return n, nil
}
