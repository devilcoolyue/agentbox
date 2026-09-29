package dockerx

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestBrowserSandboxOptIn(t *testing.T) {
	cc := &container.Config{Labels: map[string]string{}}
	hc := &container.HostConfig{SecurityOpt: []string{"no-new-privileges:true"}}
	browserContainerOptions(cc, hc, nil)
	if len(hc.SecurityOpt) != 1 || len(cc.Labels) != 0 {
		t.Fatal("changed ordinary container")
	}
	browserContainerOptions(cc, hc, map[string]string{"agentbox.browser": "1"})
	if cc.Labels["agentbox.browser"] != "1" || len(hc.SecurityOpt) != 2 || !strings.HasPrefix(hc.SecurityOpt[1], "seccomp={") {
		t.Fatal("missing sandbox profile")
	}
	if hc.Privileged || len(hc.CapAdd) > 0 || len(hc.PortBindings) > 0 {
		t.Fatal("browser exposes privileged/public access")
	}
	if strings.Contains(hc.SecurityOpt[1], "unconfined") {
		t.Fatal("seccomp disabled")
	}
}
