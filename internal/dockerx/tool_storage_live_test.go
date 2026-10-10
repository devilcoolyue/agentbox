package dockerx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/internal/toolstore"
	"github.com/docker/docker/api/types/container"
)

func TestToolStorageLive(t *testing.T) {
	ref, root := os.Getenv("AGENTBOX_TOOL_STORAGE_TEST_IMAGE"), os.Getenv("AGENTBOX_TOOL_STORAGE_TEST_ROOT")
	if ref == "" || root == "" {
		t.Skip("use scripts/test-tool-storage-live.py with an isolated Linux volume")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	data := filepath.Join(root, "data")
	tools := filepath.Join(root, "tools")
	for _, p := range []string{data, tools, filepath.Join(tools, "public")} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	cfgfile := filepath.Join(root, "config.json")
	raw, _ := json.Marshal(map[string]any{"auth_token": "synthetic-tool-storage-test", "data_dir": data, "agent_image": ref, "max_upload_mb": 10, "tool_storage": map[string]string{"root": tools}, "container": map[string]any{"network": "none", "memory_mb": 512, "cpus": 1}})
	if err := os.WriteFile(cfgfile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgfile)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetToolCacheIdentity(func(user string) (string, error) {
		return toolstore.OwnerKey("fixture-instance", user, "created-1"), nil
	})
	plan, err := m.toolPlan(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	revision := filepath.Join(tools, "public/playwright", plan.platform, "chromium-1208")
	if err = os.MkdirAll(revision, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(revision, "INSTALLATION_COMPLETE"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(revision, "shared-fixture"), []byte("public-browser"), 0644); err != nil {
		t.Fatal(err)
	}

	imageInfo, err := m.cli.ImageInspect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(tools, "public/go/linux-"+imageInfo.Architecture+"/go1.26.9")
	if err = os.MkdirAll(filepath.Join(sdk, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(sdk, "bin/go"), []byte("#!/bin/sh\necho shared-go-fixture\n"), 0755); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]any{"format": 1, "kind": "go", "version": "go1.26.9", "platform": "linux-" + imageInfo.Architecture})
	if err = os.WriteFile(filepath.Join(sdk, ".agentbox-tool.json"), metadata, 0644); err != nil {
		t.Fatal(err)
	}
	if err = cfg.SetToolStorage(config.ToolStorage{Root: tools, GoVersion: "go1.26.9"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	t.Cleanup(func() {
		cleanup, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		for _, id := range ids {
			if e := m.Remove(cleanup, id); e != nil {
				t.Error(e)
			}
		}
	})
	sessions := []store.Session{{ID: "tools-fixture-alice-one", User: "alice"}, {ID: "tools-fixture-alice-two", User: "alice"}, {ID: "tools-fixture-bob", User: "bob"}}
	starts := func(s store.Session) string {
		t.Helper()
		base := filepath.Join(data, s.ID)
		for _, part := range []string{"workspace", "home", "shared"} {
			p := filepath.Join(base, part)
			if e := os.MkdirAll(p, 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.Chown(p, AgentUID, AgentGID); e != nil {
				t.Fatal(e)
			}
		}
		cid, e := m.EnsureRunning(ctx, s, config.Account{}, filepath.Join(base, "workspace"), filepath.Join(base, "home"), filepath.Join(base, "shared"))
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, cid)
		return cid
	}
	for i := range sessions {
		sessions[i].ContainerID = starts(sessions[i])
	}
	exec := func(cid, script string) string {
		t.Helper()
		out, e := m.ExecCommand(ctx, cid, []string{"/usr/bin/python3", "-I", "-c", script})
		if e != nil {
			t.Fatal(e)
		}
		return strings.TrimSpace(out)
	}
	script := `import os,pathlib,subprocess
assert subprocess.check_output(["go","version"],text=True).strip()=="shared-go-fixture"
p=pathlib.Path(os.environ['PLAYWRIGHT_BROWSERS_PATH'])
assert (p/'chromium-1208').is_symlink()
assert (p/'chromium-1208/shared-fixture').read_text()=='public-browser'
try: (p/'chromium-1208/shared-fixture').write_text('poison')
except OSError: pass
else: raise Exception('public tool is writable')
pathlib.Path(os.environ['GOCACHE'],'private-proof').write_text('alice')
(p/'chromium-9999').mkdir()
(p/'chromium-9999/private-fixture').write_text('private fallback')
print('ok')`
	if exec(ids[0], script) != "ok" {
		t.Fatal("first user verification failed")
	}
	if exec(ids[1], `import os,pathlib; assert pathlib.Path(os.environ['GOCACHE'],'private-proof').read_text()=='alice';assert pathlib.Path(os.environ['PLAYWRIGHT_BROWSERS_PATH'],'chromium-9999/private-fixture').read_text()=='private fallback';print('ok')`) != "ok" {
		t.Fatal("same user did not share")
	}
	if exec(ids[2], `import os,pathlib; assert not pathlib.Path(os.environ['GOCACHE'],'private-proof').exists();assert not pathlib.Path(os.environ['PLAYWRIGHT_BROWSERS_PATH'],'chromium-9999').exists();print('ok')`) != "ok" {
		t.Fatal("cross-user disclosure")
	}
	if err = cfg.SetToolStorage(config.ToolStorage{}); err != nil {
		t.Fatal(err)
	}
	if cid := starts(sessions[0]); cid != sessions[0].ContainerID {
		t.Fatal("changed running container")
	}
	if err = m.cli.ContainerStop(ctx, sessions[0].ContainerID, container.StopOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh := starts(sessions[0])
	if fresh == sessions[0].ContainerID {
		t.Fatal("disabled mounts were not removed on restart")
	}
	inspect, err := m.cli.ContainerInspect(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !(toolMountPlan{}).matches(inspect) {
		t.Fatal("old shared mounts remain")
	}
	t.Log("Public read-only sharing, private fallback, same-user reuse, cross-user isolation, running-container preservation and stopped-container recreation verified")
}
