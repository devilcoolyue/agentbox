package dockerx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"agentbox/internal/netaccess"
	"agentbox/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

func networkName(cid string) string {
	if len(cid) > 12 {
		cid = cid[:12]
	}
	return "agentbox-net-" + cid
}

// EnsureNetwork is called under the workspace lifecycle lock. The sidecar has
// no workspace/home/shared mounts and cannot change agent filesystem content.
func (m *Manager) EnsureNetwork(ctx context.Context, sess store.Session, image string, cfg netaccess.HelperConfig) error {
	workspace, err := m.cli.ContainerInspect(ctx, sess.ContainerID)
	if err != nil {
		return err
	}
	if workspace.Config == nil || workspace.Config.User != execUser || workspace.HostConfig == nil || workspace.NetworkSettings == nil {
		return fmt.Errorf("透明网络要求普通用户的独立 bridge 工作空间")
	}
	mode := workspace.HostConfig.NetworkMode
	if mode.IsHost() || mode.IsNone() || mode.IsContainer() {
		return fmt.Errorf("透明网络不支持已有工作空间的网络模式，请停止后按 bridge 配置重建")
	}
	for name := range workspace.NetworkSettings.Networks {
		nw, err := m.cli.NetworkInspect(ctx, name, network.InspectOptions{})
		if err != nil {
			return err
		}
		if nw.Driver != "bridge" {
			return fmt.Errorf("透明网络仅支持 Docker bridge，当前为 %s", nw.Driver)
		}
	}
	name := networkName(sess.ContainerID)
	sum := sha256.Sum256([]byte(image + "\x00" + cfg.Control + "\x00" + cfg.Secret))
	fingerprint := hex.EncodeToString(sum[:])
	info, err := m.cli.ContainerInspect(ctx, name)
	if err == nil {
		if info.Config == nil || info.Config.Labels["agentbox.network"] != sess.ID {
			return fmt.Errorf("network helper name conflict")
		}
		if info.Config.Labels["agentbox.network.config"] == fingerprint && info.State != nil && info.State.Running {
			return nil
		}
		if err = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
			return err
		}
	} else if !client.IsErrNotFound(err) {
		return err
	}
	pids := int64(64)
	cc := &container.Config{Image: image, User: "0:0", Env: []string{"ABOX_NETWORK_CONTROL=" + cfg.Control, "ABOX_NETWORK_SESSION=" + cfg.Session, "ABOX_NETWORK_SECRET=" + cfg.Secret}, Labels: map[string]string{"agentbox.network": sess.ID, "agentbox.network.config": fingerprint}}
	hc := &container.HostConfig{NetworkMode: container.NetworkMode("container:" + sess.ContainerID), CapDrop: []string{"ALL"}, CapAdd: []string{"NET_ADMIN", "NET_RAW"}, SecurityOpt: []string{"no-new-privileges:true"}, ReadonlyRootfs: true, RestartPolicy: container.RestartPolicy{Name: "unless-stopped"}, Resources: container.Resources{Memory: 128 << 20, NanoCPUs: 500000000, PidsLimit: &pids}, Tmpfs: map[string]string{"/run": "rw,noexec,nosuid,size=1m"}}
	resp, err := m.cli.ContainerCreate(ctx, cc, hc, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create network helper (build %s first): %w", image, err)
	}
	if err = m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start network helper: %w", err)
	}
	return nil
}
func (m *Manager) removeNetwork(ctx context.Context, cid string) error {
	if cid == "" {
		return nil
	}
	name := networkName(cid)
	info, err := m.cli.ContainerInspect(ctx, name)
	if client.IsErrNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Config == nil || info.Config.Labels["agentbox.network"] == "" {
		return fmt.Errorf("unmanaged network helper name conflict")
	}
	err = m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	if client.IsErrNotFound(err) {
		return nil
	}
	return err
}
