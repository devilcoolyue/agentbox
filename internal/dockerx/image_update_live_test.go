package dockerx

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/imageupdate"
	"github.com/docker/docker/api/types/image"
)

// Opt-in Linux Docker build smoke. Installs the selected public npm package;
// never mounts credentials, contacts a model, or changes the configured image.
func TestCLIImageBuildLive(t *testing.T) {
	baseRef := os.Getenv("AGENTBOX_CLI_UPDATE_TEST_IMAGE")
	claude := os.Getenv("AGENTBOX_CLI_UPDATE_TEST_CLAUDE")
	if baseRef == "" || claude == "" {
		t.Skip("set AGENTBOX_CLI_UPDATE_TEST_IMAGE and AGENTBOX_CLI_UPDATE_TEST_CLAUDE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	m, err := New(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	base, err := m.InspectCLIImage(ctx, baseRef)
	if err != nil {
		t.Fatal(err)
	}
	before, err := m.cli.ImageInspect(ctx, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("agentbox-cli-update-test:%d", time.Now().UnixNano())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = m.cli.ImageRemove(cleanup, tag, image.RemoveOptions{})
	}()
	if err := m.BuildCLIImage(ctx, base, imageupdate.Versions{Claude: claude, Codex: base.Codex}, tag, func(s string) { t.Log(s) }); err != nil {
		t.Fatal(err)
	}
	built, err := m.InspectCLIImage(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if built.Claude != claude || built.Codex != base.Codex || built.Browser != base.Browser || built.User != base.User {
		t.Fatalf("metadata changed unexpectedly: %+v", built)
	}
	after, err := m.cli.ImageInspect(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Config.Env, after.Config.Env) || !reflect.DeepEqual(before.Config.Cmd, after.Config.Cmd) || !reflect.DeepEqual(before.Config.Entrypoint, after.Config.Entrypoint) || !reflect.DeepEqual(before.Config.Shell, after.Config.Shell) {
		t.Fatal("base execution configuration changed")
	}
	for key, value := range before.Config.Labels {
		if key != "agentbox.claude-code" && after.Config.Labels[key] != value {
			t.Fatalf("lost base label %s", key)
		}
	}
	t.Logf("Verified Claude %s, Codex %s, browser=%v and base config preservation", built.Claude, built.Codex, built.Browser)
}
