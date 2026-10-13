package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"agentbox/internal/agentprobe"
	"agentbox/internal/modelcatalog"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// probeMessages are the candidate gate's texts for sandbox failure codes.
var probeMessages = map[string]string{
	"image_identity_invalid": "候选镜像 ID 无效",
	"probe_image_invalid":    "候选镜像无法用于隔离验证",
	"probe_create_failed":    "无法创建候选验证容器",
	"probe_start_failed":     "无法启动候选验证容器",
	"probe_logs_failed":      "无法读取候选验证结果",
	"probe_output_failed":    "候选验证输出无效或超时",
	"probe_wait_failed":      "候选验证未正常结束",
	"probe_cancelled":        "候选验证已取消或超时",
	"probe_cleanup_failed":   "候选验证容器清理失败，未切换镜像",
}

// ValidateCLIImage runs immutable candidate bytes in an owned, offline sandbox.
// No account/env, daemon socket, workspace, bind mount or production store is passed.
func (m *Manager) ValidateCLIImage(parent context.Context, imageID string) (agentprobe.Report, error) {
	report := agentprobe.Report{Version: agentprobe.Version, ImageID: imageID, CheckedAt: time.Now().UnixMilli()}
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	output, exitCode, failure := m.runOfflineNode(ctx, imageID, "agentbox.cli-probe", agentprobe.Script, string(agentprobe.Input()), 4<<20)
	if failure != "" {
		report.Failure = failure
		if failure == "probe_logs_failed" {
			report.Failure = "probe_output_failed"
		}
		return report, errors.New(probeMessages[failure])
	}
	report = agentprobe.Validate(ctx, imageID, output)
	if exitCode != 0 && report.Failure == "" {
		report.Failure = "probe_exit_failed"
	}
	if !report.Passed(imageID) {
		return report, fmt.Errorf("候选 CLI 协议验证失败（%s），未切换镜像", report.Failure)
	}
	return report, nil
}

// ReadCLICatalog asks the image's CLIs, signed out and offline in the same
// sandbox as the candidate gate, which models they offer (modelcatalog.Script).
func (m *Manager) ReadCLICatalog(parent context.Context, imageID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	output, _, failure := m.runOfflineNode(ctx, imageID, "agentbox.cli-catalog", modelcatalog.Script, "", 1<<20)
	if failure != "" {
		return nil, fmt.Errorf("读取 CLI 内置模型目录失败（%s）", failure)
	}
	return output, nil
}

// runOfflineNode runs `node -e script arg` from an immutable image ID in a
// disposable container: no network, image ENV, mounts or daemon socket;
// read-only root, UID 1000, dropped capabilities and resource limits. The
// container is always removed; failure is a stable code, "" on success.
func (m *Manager) runOfflineNode(ctx context.Context, imageID, label, script, arg string, limit int) (output []byte, exitCode int64, failure string) {
	if !imageIDRE.MatchString(imageID) {
		return nil, 0, "image_identity_invalid"
	}
	info, e := m.cli.ImageInspect(ctx, imageID)
	if e != nil || info.Config == nil || len(info.Config.Volumes) > 0 {
		return nil, 0, "probe_image_invalid"
	}
	// Docker otherwise inherits the image's ENV. Clear even custom baked env;
	// child CLIs receive a second explicit allowlist inside the embedded driver.
	env := map[string]string{}
	for _, entry := range info.Config.Env {
		key, _, _ := strings.Cut(entry, "=")
		env[key] = ""
	}
	env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	env["HOME"] = "/tmp"
	env["LANG"] = "C.UTF-8"
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+env[key])
	}
	cmd := []string{"-e", script}
	if arg != "" {
		cmd = append(cmd, arg)
	}
	init := true
	pids := int64(128)
	created, e := m.cli.ContainerCreate(ctx, &container.Config{
		Image: imageID, User: "1000:1000", WorkingDir: "/tmp", NetworkDisabled: true,
		Entrypoint: []string{"/usr/local/bin/node"}, Cmd: cmd, Env: values,
		Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}, Labels: map[string]string{label: "1"},
	}, &container.HostConfig{
		NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, Init: &init,
		Tmpfs:     map[string]string{"/tmp": "rw,nosuid,nodev,size=256m,mode=1777", "/home/agent": "rw,nosuid,nodev,size=64m,uid=1000,gid=1000", "/workspace": "rw,nosuid,nodev,size=64m,uid=1000,gid=1000"},
		Resources: container.Resources{Memory: 1 << 30, NanoCPUs: 2_000_000_000, PidsLimit: &pids},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "4m", "max-file": "1"}},
	}, nil, nil, "")
	if e != nil {
		return nil, 0, "probe_create_failed"
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if e := m.cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); e != nil {
			output, failure = nil, "probe_cleanup_failed"
		}
	}()
	if e = m.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); e != nil {
		return nil, 0, "probe_start_failed"
	}
	logs, e := m.cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if e != nil {
		return nil, 0, "probe_logs_failed"
	}
	defer logs.Close()
	out := &probeOutput{limit: limit}
	if _, e = stdcopy.StdCopy(out, io.Discard, logs); e != nil {
		return nil, 0, "probe_output_failed"
	}
	wait, waitErr := m.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case state := <-wait:
		return out.Bytes(), state.StatusCode, ""
	case <-waitErr:
		return nil, 0, "probe_wait_failed"
	case <-ctx.Done():
		return nil, 0, "probe_cancelled"
	}
}

type probeOutput struct {
	bytes.Buffer
	limit int
}

func (w *probeOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > w.limit {
		return 0, errors.New("probe output limit")
	}
	return w.Buffer.Write(p)
}
