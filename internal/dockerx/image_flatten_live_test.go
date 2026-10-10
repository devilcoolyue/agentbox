package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/imageupdate"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// Real Docker filesystem/config/GC regression with synthetic CLI executables.
// No provider, accounts, workspace mounts, registry download or global prune.
func TestCLIImageMaintenanceLive(t *testing.T)    { runCLIImageMaintenanceLive(t, true) }
func TestCLIImageFixedBaseBuildLive(t *testing.T) { runCLIImageMaintenanceLive(t, false) }
func runCLIImageMaintenanceLive(t *testing.T, flatten bool) {
	ref := os.Getenv("AGENTBOX_CLI_MAINTENANCE_TEST_IMAGE")
	if ref == "" {
		t.Skip("set AGENTBOX_CLI_MAINTENANCE_TEST_IMAGE to a local Linux Agent image with node/python3")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	m, err := New(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	source, err := m.cli.ImageInspect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("agentbox-cli-maintenance-test:%d", time.Now().UnixNano())
	var tags []string
	var containers []string
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, id := range containers {
			if e := m.cli.ContainerRemove(cleanup, id, container.RemoveOptions{Force: true}); e != nil && !client.IsErrNotFound(e) {
				t.Errorf("cleanup container: %v", e)
			}
		}
		for i := len(tags) - 1; i >= 0; i-- {
			if _, e := m.cli.ImageRemove(cleanup, tags[i], image.RemoveOptions{Force: false, PruneChildren: true}); e != nil && !client.IsErrNotFound(e) {
				t.Errorf("cleanup image %s: %v", tags[i], e)
			}
		}
	})
	build := func(tag, df string) {
		t.Helper()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if e := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0600, Size: int64(len(df))}); e != nil {
			t.Fatal(e)
		}
		if _, e := tw.Write([]byte(df)); e != nil {
			t.Fatal(e)
		}
		if e := tw.Close(); e != nil {
			t.Fatal(e)
		}
		tags = append(tags, tag)
		r, e := m.cli.ImageBuild(ctx, &buf, types.ImageBuildOptions{Tags: []string{tag}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true, Version: types.BuilderV1})
		if e != nil {
			t.Fatal(e)
		}
		e = readBuildOutput(r.Body, func(s string) { t.Log(strings.TrimSpace(s)) })
		r.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	// Stub only test-image CLI commands. Keep real node for the isolated space
	// helper and real shell for builds. A replaced 8 MiB file exposes old-layer
	// retention even though the visible content has a constant size.
	df := fmt.Sprintf(`FROM %s
USER root
RUN rm -f /usr/local/bin/npm /usr/local/bin/claude /usr/local/bin/codex && printf '#!/bin/sh\nfor arg in "$@"; do case "$arg" in @anthropic-ai/claude-code@*) echo "${arg##*@}" > /opt/test-claude;; @openai/codex@*) echo "${arg##*@}" > /opt/test-codex;; esac; done\n' > /usr/local/bin/npm && printf '#!/bin/sh\ncat /opt/test-claude\n' > /usr/local/bin/claude && printf '#!/bin/sh\nprintf "codex-cli "; cat /opt/test-codex\n' > /usr/local/bin/codex && chmod 755 /usr/local/bin/npm /usr/local/bin/claude /usr/local/bin/codex && echo 2.1.280 > /opt/test-claude && echo 0.156.1 > /opt/test-codex
RUN python3 -c "open('/opt/flatten-data','wb').write(b'a'*(8*1024*1024))" && chown 123:456 /opt/flatten-data && chmod 640 /opt/flatten-data && ln /opt/flatten-data /opt/flatten-hard && ln -s /opt/flatten-data /opt/flatten-link && echo stable > /opt/flatten-stable && ln /opt/flatten-stable /opt/flatten-stable-hard
ENV FLATTEN_TEST="a=b c"
WORKDIR /opt
HEALTHCHECK --interval=37s CMD ["/bin/false"]
STOPSIGNAL SIGQUIT
SHELL ["/bin/sh", "-ec"]
LABEL custom.keep="yes" agentbox.claude-code="2.1.280" agentbox.codex="0.156.1"
USER 1000:1000
`, source.ID)
	build(prefix+"-one", df)
	build(prefix+"-two", fmt.Sprintf("FROM %s\nUSER root\nRUN python3 -c \"open('/opt/flatten-data','wb').write(b'b'*(8*1024*1024))\"\nUSER 1000:1000\n", prefix+"-one"))
	original, err := m.InspectCLIImage(ctx, prefix+"-two")
	if err != nil {
		t.Fatal(err)
	}

	// Overlay copy-up may split a hard link when only one member is overwritten
	// in a later layer. Compare the SOURCE's actual visible relationships, and
	// include an untouched hard-link pair which must remain linked.
	script := `const fs=require('fs');const a=fs.statSync('/opt/flatten-data'),b=fs.statSync('/opt/flatten-hard');console.log(JSON.stringify({uid:a.uid,gid:a.gid,mode:a.mode&511,size:a.size,changedPair:a.ino===b.ino,stablePair:fs.statSync('/opt/flatten-stable').ino===fs.statSync('/opt/flatten-stable-hard').ino,link:fs.readlinkSync('/opt/flatten-link'),text:fs.readFileSync('/opt/flatten-stable','utf8')}))`
	readMetadata := func(id string) string {
		t.Helper()
		out, code, failure := m.runOfflineNode(ctx, id, "agentbox.cli-maintenance-test", script, "", 2048)
		if failure != "" || code != 0 {
			t.Fatalf("metadata probe: %s %d %q", failure, code, out)
		}
		return strings.TrimSpace(string(out))
	}
	beforeFiles := readMetadata(original.ID)
	if !strings.Contains(beforeFiles, `"stablePair":true`) {
		t.Fatal("fixture does not contain a hard-link pair", beforeFiles)
	}
	t.Log("Source filesystem metadata:", beforeFiles)
	flat := original
	var after image.InspectResponse
	if flatten {
		tags = append(tags, "agentbox-agent:base-"+strings.TrimPrefix(original.ID, "sha256:"))
		flat, err = m.FlattenCLIImage(ctx, original, func(s string) { t.Log(s) })
		if err != nil {
			t.Fatal(err)
		}
		before, err := m.cli.ImageInspect(ctx, original.ID)
		if err != nil {
			t.Fatal(err)
		}
		after, err = m.cli.ImageInspect(ctx, flat.ID)
		if err != nil {
			t.Fatal(err)
		}
		delete(after.Config.Labels, flatSourceLabel)
		delete(after.Config.Labels, flatSchemaLabel)
		if !reflect.DeepEqual(before.Config, after.Config) || len(after.RootFS.Layers) != 1 || after.Size >= before.Size {
			t.Fatalf("migration lost config or retained history: before=%d after=%d layers=%d", before.Size, after.Size, len(after.RootFS.Layers))
		}
		again, err := m.FlattenCLIImage(ctx, original, func(s string) { t.Log(s) })
		if err != nil || again.ID != flat.ID {
			t.Fatalf("restart did not reuse staged image: %v", err)
		}
		if afterFiles := readMetadata(flat.ID); afterFiles != beforeFiles {
			t.Fatalf("filesystem metadata changed: before=%s after=%s", beforeFiles, afterFiles)
		}
	}
	layers := 0
	var first string
	for i := 1; i <= 3; i++ {
		tag := fmt.Sprintf("agentbox-agent:cli-%d", time.Now().UnixNano())
		tags = append(tags, tag)
		if err = m.BuildCLIImage(ctx, flat, imageupdate.Versions{Claude: fmt.Sprintf("2.1.%d", 280+i), Codex: "0.157.0"}, tag, func(s string) { t.Log(s) }); err != nil {
			t.Fatal(err)
		}
		v, e := m.cli.ImageInspect(ctx, tag)
		if e != nil {
			t.Fatal(e)
		}
		if i == 1 {
			layers = len(v.RootFS.Layers)
			first = v.ID
		} else if len(v.RootFS.Layers) != layers {
			t.Fatal("layers grew across fixed-base updates")
		}
	}
	c, err := m.cli.ContainerCreate(ctx, &container.Config{Image: first}, &container.HostConfig{NetworkMode: "none"}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	containers = append(containers, c.ID)
	if removed, e := m.RemoveCLIImage(ctx, first, map[string]bool{flat.ID: true}); e != nil || removed {
		t.Fatal("removed image referenced by a stopped container", e)
	}
	if err = m.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if removed, e := m.RemoveCLIImage(ctx, first, map[string]bool{flat.ID: true}); e != nil || !removed {
		t.Fatal("did not remove unreferenced owned image", e)
	}
	if flatten {
		t.Logf("Flat rootfs %.2f GiB; three updates each have %d layers; config/file metadata and stopped-container protection verified", float64(after.Size)/(1<<30), layers)
	} else {
		t.Logf("Three fixed-base updates each have %d layers; runtime config and stopped-container protection verified", layers)
	}
}
