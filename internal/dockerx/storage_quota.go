package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

const quotaGuardLabel = "agentbox.storage-quota"
const quotaGuardVersion = "2"

func (m *Manager) CheckStorageQuotaContainer(ctx context.Context, id string) error {
	if id == "" || m.cfg.GetStoragePolicy().Mount == "" {
		return nil
	}
	info, err := m.cli.ContainerInspect(ctx, id)
	if client.IsErrNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.State != nil && info.State.Running && (info.Config == nil || info.Config.Labels[quotaGuardLabel] != quotaGuardVersion) {
		return fmt.Errorf("stop existing workspaces before enabling filesystem quotas")
	}
	return nil
}

// Project inheritance can be cleared by the directory owner on ext4. Block
// both attribute mutation ioctls in quota-managed containers, including 32-bit
// SETFLAGS and ioctl commands with nonzero high bits. Keep the browser sandbox.
func quotaContainerOptions(cc *container.Config, hc *container.HostConfig) error {
	var profile map[string]any
	if err := json.Unmarshal([]byte(browserSeccomp), &profile); err != nil {
		return err
	}
	calls := profile["syscalls"].([]any)
	if cc.Labels["agentbox.browser"] != "1" {
		calls = calls[1:]
	} // remove only Playwright's namespace exception

	// libseccomp's unconditional ioctl allow subsumes narrower errno rules.
	// Replace that allow with disjoint prefix matches for the complement of the
	// three forbidden low-32-bit commands. Each rule compares the argument once.
	filtered := []any{}
	for _, raw := range calls {
		call := raw.(map[string]any)
		names := []any{}
		for _, name := range call["names"].([]any) {
			if name != "ioctl" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			call["names"] = names
			filtered = append(filtered, call)
		}
	}
	calls = filtered
	var allow func(uint64, uint64, int, []uint64)
	allow = func(prefix, mask uint64, bit int, blocked []uint64) {
		if len(blocked) == 0 {
			calls = append(calls, map[string]any{"names": []string{"ioctl"}, "action": "SCMP_ACT_ALLOW", "args": []any{map[string]any{"index": 1, "value": mask, "valueTwo": prefix, "op": "SCMP_CMP_MASKED_EQ"}}})
			return
		}
		if bit < 0 {
			return
		}
		flag := uint64(1) << bit
		for _, value := range []uint64{0, flag} {
			subset := []uint64{}
			for _, cmd := range blocked {
				if cmd&flag == value {
					subset = append(subset, cmd)
				}
			}
			allow(prefix|value, mask|flag, bit-1, subset)
		}
	}
	allow(0, 0, 31, []uint64{0x401c5820, 0x40086602, 0x40046602})

	profile["syscalls"] = calls
	raw, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	opts := []string{}
	for _, option := range hc.SecurityOpt {
		if !strings.HasPrefix(option, "seccomp=") {
			opts = append(opts, option)
		}
	}
	hc.SecurityOpt = append(opts, "seccomp="+string(raw))
	cc.Labels[quotaGuardLabel] = quotaGuardVersion
	return nil
}
