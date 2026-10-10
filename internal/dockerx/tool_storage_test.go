package dockerx

import (
	"agentbox/internal/toolstore"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"testing"
)

func TestToolMountPlanRequiresReadOnlyPublicAndExactOwner(t *testing.T) {
	plan := toolMountPlan{revision: "v1", mounts: []mount.Mount{{Source: "/tools/public", Target: toolstore.PublicMount, ReadOnly: true}, {Source: "/tools/private/alice/cache", Target: toolstore.CacheMount}}}
	info := container.InspectResponse{Config: &container.Config{Labels: map[string]string{toolStorageLabel: "v1"}}, Mounts: []container.MountPoint{{Source: "/tools/public", Destination: toolstore.PublicMount, RW: false}, {Source: "/tools/private/alice/cache", Destination: toolstore.CacheMount, RW: true}}}
	if !plan.matches(info) {
		t.Fatal("valid tool mounts rejected")
	}
	info.Mounts[0].RW = true
	if plan.matches(info) {
		t.Fatal("public writable")
	}
	info.Mounts[0].RW = false
	info.Mounts[1].Source = "/tools/private/bob/cache"
	if plan.matches(info) {
		t.Fatal("wrong user mount")
	}
	if (toolMountPlan{}).matches(info) {
		t.Fatal("disabled tool storage reused old mounts")
	}
	for _, key := range toolEnvKeys {
		if hasBakedEnv(append(baseContainerEnv(), key+"=value"), nil) {
			t.Fatal("tool env mistaken for credentials", key)
		}
	}
	if !hasBakedEnv(append(baseContainerEnv(), "ANTHROPIC_AUTH_TOKEN=token"), nil) {
		t.Fatal("credential env accepted")
	}
}
