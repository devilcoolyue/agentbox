package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"agentbox/internal/imageupdate"
	"github.com/docker/docker/api/types"
)

func (m *Manager) InspectCLIImage(ctx context.Context, ref string) (imageupdate.Image, error) {
	info, err := m.cli.ImageInspect(ctx, ref)
	if err != nil {
		return imageupdate.Image{}, fmt.Errorf("读取本地 Agent 镜像失败: %w", err)
	}
	if info.Config == nil {
		return imageupdate.Image{}, fmt.Errorf("镜像缺少配置")
	}
	if len(info.Config.OnBuild) > 0 {
		return imageupdate.Image{}, fmt.Errorf("不支持带 ONBUILD 指令的镜像")
	}
	return imageupdate.Image{ID: info.ID, Claude: info.Config.Labels["agentbox.claude-code"], Codex: info.Config.Labels["agentbox.codex"], Browser: info.Config.Labels["agentbox.browser"] == "1", User: info.Config.User}, nil
}

var imageIDRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var imageUserRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]+(:[a-zA-Z0-9_.-]+)?$`)

func cliDockerfile(base imageupdate.Image, versions imageupdate.Versions) (string, error) {
	if !imageIDRE.MatchString(base.ID) || !imageupdate.ValidVersion(versions.Claude) || !imageupdate.ValidVersion(versions.Codex) {
		return "", fmt.Errorf("镜像 ID 或 CLI 版本不合法")
	}
	user := base.User
	if user == "" {
		user = "root"
	}
	if !imageUserRE.MatchString(user) {
		return "", fmt.Errorf("不支持的镜像 USER")
	}
	// Exec-form RUN leaves inherited SHELL, entrypoint, environment and browser
	// layers intact. Only selected CLI packages and their version labels change.
	packages := ""
	if versions.Claude != base.Claude {
		packages += " @anthropic-ai/claude-code@" + versions.Claude
	}
	if versions.Codex != base.Codex {
		packages += " @openai/codex@" + versions.Codex
	}
	if packages == "" {
		return "", fmt.Errorf("没有需要更新的 CLI")
	}
	run := func(command string) string {
		raw, _ := json.Marshal([]string{"/bin/sh", "-c", command})
		return "RUN " + string(raw) + "\n"
	}
	text := fmt.Sprintf("FROM %s\nUSER root\n", base.ID)
	text += run("npm install -g --registry=https://registry.npmjs.org" + packages + " && npm cache clean --force")
	text += run(fmt.Sprintf(`test "$(DISABLE_AUTOUPDATER=1 claude --version | cut -d ' ' -f 1)" = "%s" && test "$(codex --version | cut -d ' ' -f 2)" = "%s"`, versions.Claude, versions.Codex))
	if base.Browser {
		text += run("test -x /usr/local/bin/agentbox-browser && test -f /opt/agentbox/browser.py")
	}
	text += fmt.Sprintf("LABEL agentbox.claude-code=\"%s\" agentbox.codex=\"%s\"\nUSER %s\n", versions.Claude, versions.Codex, user)
	return text, nil
}

func (m *Manager) BuildCLIImage(ctx context.Context, base imageupdate.Image, versions imageupdate.Versions, tag string, log func(string)) error {
	dockerfile, err := cliDockerfile(base, versions)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0600, Size: int64(len(dockerfile))}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	res, err := m.cli.ImageBuild(ctx, &buf, types.ImageBuildOptions{Tags: []string{tag}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true, Version: types.BuilderV1})
	if err != nil {
		return fmt.Errorf("构建镜像失败: %w", err)
	}
	defer res.Body.Close()
	return readBuildOutput(res.Body, log)
}

func readBuildOutput(r io.Reader, log func(string)) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Stream      string `json:"stream"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("读取构建日志失败: %w", err)
		}
		if msg.Stream != "" {
			log(msg.Stream)
		}
		if msg.Error != "" {
			return fmt.Errorf("镜像构建失败: %s", msg.Error)
		}
		if msg.ErrorDetail.Message != "" {
			return fmt.Errorf("镜像构建失败: %s", msg.ErrorDetail.Message)
		}
	}
}
