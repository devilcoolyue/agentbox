package dockerx

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

//go:embed browser-seccomp.json
var browserSeccomp string

// BrowserInfo never exposes the internal proxy credential or network hash.
type BrowserInfo struct {
	Available  bool   `json:"available"`
	Running    bool   `json:"running"`
	Browser    string `json:"browser,omitempty"`
	Proxy      bool   `json:"proxy"`
	NetworkKey string `json:"-"`
}

func (m *Manager) BrowserCommand(ctx context.Context, id, action, url string, env []string) (BrowserInfo, error) {
	info, err := m.cli.ContainerInspect(ctx, id)
	if err != nil {
		return BrowserInfo{}, err
	}
	available := info.Config != nil && info.Config.Labels["agentbox.browser"] == "1"
	if !available {
		return BrowserInfo{}, nil
	}
	payload, _ := json.Marshal(map[string]string{"url": url})
	out, err := m.ExecCapture(ctx, id, []string{"python3", "-I", "/opt/agentbox/browser.py", action}, env, string(payload))
	if err != nil {
		return BrowserInfo{}, fmt.Errorf("浏览器操作失败，请检查镜像、内存和 Chrome 沙箱支持: %w", err)
	}
	var result struct {
		Running    bool   `json:"running"`
		Browser    string `json:"browser"`
		Proxy      bool   `json:"proxy"`
		NetworkKey string `json:"network_key"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return BrowserInfo{}, errors.New("浏览器返回无效状态")
	}
	if result.Error != "" {
		return BrowserInfo{}, errors.New("浏览器操作失败")
	}
	return BrowserInfo{Available: true, Running: result.Running, Browser: result.Browser, Proxy: result.Proxy, NetworkKey: result.NetworkKey}, nil
}

func browserContainerOptions(cc *container.Config, hc *container.HostConfig, labels map[string]string) {
	if labels["agentbox.browser"] != "1" {
		return
	}
	cc.Labels["agentbox.browser"] = "1"
	// Docker's default seccomp denies the user namespaces required by Chrome's
	// sandbox. Preserve all other default restrictions; never use --no-sandbox.
	var compact json.RawMessage = json.RawMessage(browserSeccomp)
	raw, _ := json.Marshal(compact)
	hc.SecurityOpt = append(hc.SecurityOpt, "seccomp="+string(raw))
	hc.ShmSize = 256 << 20
}

func (m *Manager) BrowserClipboard(ctx context.Context, id string, write bool, text string) (string, error) {
	payload, _ := json.Marshal(map[string]any{"write": write, "text": text})
	out, err := m.ExecCapture(ctx, id, []string{"python3", "-I", "/opt/agentbox/browser.py", "clipboard"}, nil, string(payload))
	if err != nil {
		return "", err
	}
	var result struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return "", err
	}
	if result.Error != "" {
		return "", errors.New("剪贴板操作失败")
	}
	return result.Text, nil
}

// Flush browser state before stopping/removing the workspace. Docker's stop
// signal targets PID 1 (sleep/tini), not Chrome's window manager, so it cannot
// by itself guarantee that the browser persists its most recent cookies.
func (m *Manager) closeBrowser(ctx context.Context, id string) error {
	info, err := m.cli.ContainerInspect(ctx, id)
	if client.IsErrNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.State == nil || !info.State.Running || info.Config == nil || info.Config.Labels["agentbox.browser"] != "1" {
		return nil
	}
	_, err = m.BrowserCommand(ctx, id, "stop", "", nil)
	return err
}
