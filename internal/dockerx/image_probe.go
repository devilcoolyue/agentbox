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
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// ValidateCLIImage runs immutable candidate bytes in an owned, offline sandbox.
// No account/env, daemon socket, workspace, bind mount or production store is passed.
func (m *Manager) ValidateCLIImage(parent context.Context, imageID string) (report agentprobe.Report, err error) {
	report = agentprobe.Report{Version: agentprobe.Version, ImageID: imageID, CheckedAt: time.Now().UnixMilli()}
	if !imageIDRE.MatchString(imageID) {
		report.Failure = "image_identity_invalid"
		return report, errors.New("候选镜像 ID 无效")
	}
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	info, e := m.cli.ImageInspect(ctx, imageID)
	if e != nil || info.Config == nil || len(info.Config.Volumes) > 0 {
		report.Failure = "probe_image_invalid"
		return report, errors.New("候选镜像无法用于隔离验证")
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
	init := true
	pids := int64(128)
	created, e := m.cli.ContainerCreate(ctx, &container.Config{
		Image: imageID, User: "1000:1000", WorkingDir: "/tmp", NetworkDisabled: true,
		Entrypoint: []string{"/usr/local/bin/node"}, Cmd: []string{"-e", agentprobe.Script, string(agentprobe.Input())}, Env: values,
		Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}, Labels: map[string]string{"agentbox.cli-probe": "1"},
	}, &container.HostConfig{
		NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, Init: &init,
		Tmpfs:     map[string]string{"/tmp": "rw,nosuid,nodev,size=256m,mode=1777", "/home/agent": "rw,nosuid,nodev,size=64m,uid=1000,gid=1000", "/workspace": "rw,nosuid,nodev,size=64m,uid=1000,gid=1000"},
		Resources: container.Resources{Memory: 1 << 30, NanoCPUs: 2_000_000_000, PidsLimit: &pids},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "4m", "max-file": "1"}},
	}, nil, nil, "")
	if e != nil {
		report.Failure = "probe_create_failed"
		return report, errors.New("无法创建候选验证容器")
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if e := m.cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); e != nil {
			report.Failure = "probe_cleanup_failed"
			err = errors.New("候选验证容器清理失败，未切换镜像")
		}
	}()
	if e = m.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); e != nil {
		report.Failure = "probe_start_failed"
		return report, errors.New("无法启动候选验证容器")
	}
	logs, e := m.cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if e != nil {
		report.Failure = "probe_output_failed"
		return report, errors.New("无法读取候选验证结果")
	}
	defer logs.Close()
	output := &probeOutput{limit: 4 << 20}
	if _, e = stdcopy.StdCopy(output, io.Discard, logs); e != nil {
		report.Failure = "probe_output_failed"
		return report, errors.New("候选验证输出无效或超时")
	}

	wait, waitErr := m.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var exitCode int64
	select {
	case state := <-wait:
		exitCode = state.StatusCode
	case <-waitErr:
		report.Failure = "probe_wait_failed"
		return report, errors.New("候选验证未正常结束")
	case <-ctx.Done():
		report.Failure = "probe_cancelled"
		return report, errors.New("候选验证已取消或超时")
	}
	report = agentprobe.Validate(ctx, imageID, output.Bytes())
	if exitCode != 0 && report.Failure == "" {
		report.Failure = "probe_exit_failed"
	}
	if !report.Passed(imageID) {
		return report, fmt.Errorf("候选 CLI 协议验证失败（%s），未切换镜像", report.Failure)
	}
	return report, nil
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
