package dockerx

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"agentbox/internal/toolstore"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

//go:embed tool_storage.py
var toolStorageInit string

const toolStorageLabel = "agentbox.tool-storage"

var toolPlatformRE = regexp.MustCompile(`^linux-(amd64|arm64)-[a-z0-9]+-[a-z0-9.]+$`)
var toolEnvKeys = []string{"GOCACHE", "GOMODCACHE", "npm_config_cache", "PLAYWRIGHT_BROWSERS_PATH", "PLAYWRIGHT_SKIP_BROWSER_GC", "PUPPETEER_CACHE_DIR"}

// SetToolCacheIdentity is called once before serving. The identity includes the
// instance and user creation identity, so deleted/recreated names cannot inherit
// old private modules or cache content.
func (m *Manager) SetToolCacheIdentity(resolve func(string) (string, error)) {
	m.toolIdentity = resolve
}

type toolMountPlan struct {
	mounts             []mount.Mount
	env                []string
	revision, platform string
}

func (m *Manager) toolPlan(ctx context.Context, user string) (toolMountPlan, error) {
	p := m.cfg.GetToolStorage()
	if p.Root == "" {
		return toolMountPlan{}, nil
	}
	if m.toolIdentity == nil {
		return toolMountPlan{}, fmt.Errorf("tool cache owner identity unavailable")
	}
	key, err := m.toolIdentity(user)
	if err != nil {
		return toolMountPlan{}, err
	}
	paths, err := toolstore.Prepare(p.Root, key, AgentUID, AgentGID)
	if err != nil {
		return toolMountPlan{}, err
	}
	img, err := m.cli.ImageInspect(ctx, m.cfg.GetAgentImage())
	if err != nil {
		return toolMountPlan{}, err
	}
	if img.Config == nil || img.Os != "linux" {
		return toolMountPlan{}, fmt.Errorf("shared tools require a Linux image")
	}
	var platform string
	if cached, ok := m.toolPlatforms.Load(img.ID); ok {
		platform = cached.(string)
	} else {
		probe, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, code, failure := m.runOfflineNode(probe, img.ID, "agentbox.tool-platform", `const fs=require('fs'),os=require('os');const d={};for(const line of fs.readFileSync('/etc/os-release','utf8').split('\n')){const m=line.match(/^([A-Z_]+)=(.*)$/);if(m)d[m[1]]=m[2].replace(/^"|"$/g,'');}const a={x64:'amd64',arm64:'arm64'}[os.arch()];console.log('linux-'+a+'-'+d.ID+'-'+d.VERSION_ID);`, "", 256)
		platform = strings.TrimSpace(string(out))
		if failure != "" || code != 0 || !toolPlatformRE.MatchString(platform) {
			return toolMountPlan{}, fmt.Errorf("cannot identify shared-tool runtime platform")
		}
		m.toolPlatforms.Store(img.ID, platform)
	}
	bin, err := toolstore.GoBin(p.Root, img.Architecture, p.GoVersion)
	if err != nil {
		return toolMountPlan{}, err
	}
	cache := toolstore.CacheMount + "/" + platform
	env := []string{"GOCACHE=" + cache + "/go-build", "GOMODCACHE=" + cache + "/go-mod", "npm_config_cache=" + cache + "/npm", "PLAYWRIGHT_BROWSERS_PATH=" + cache + "/playwright", "PLAYWRIGHT_SKIP_BROWSER_GC=1", "PUPPETEER_CACHE_DIR=" + cache + "/puppeteer"}
	if bin != "" {
		path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
		for _, v := range img.Config.Env {
			if strings.HasPrefix(v, "PATH=") {
				path = strings.TrimPrefix(v, "PATH=")
			}
		}
		env = append(env, "PATH="+bin+":"+path)
	}
	sum := sha256.Sum256([]byte(toolstore.Layout + "\x00" + paths.Public + "\x00" + paths.Cache + "\x00" + strings.Join(env, "\x00")))
	return toolMountPlan{mounts: []mount.Mount{
		{Type: mount.TypeBind, Source: paths.Public, Target: toolstore.PublicMount, ReadOnly: true},
		{Type: mount.TypeBind, Source: paths.Cache, Target: toolstore.CacheMount},
	}, env: env, revision: hex.EncodeToString(sum[:]), platform: platform}, nil
}
func (plan toolMountPlan) matches(info container.InspectResponse) bool {
	if info.Config == nil {
		return false
	}
	if info.Config.Labels[toolStorageLabel] != plan.revision {
		return false
	}
	for _, expected := range plan.mounts {
		found := false
		for _, actual := range info.Mounts {
			if actual.Destination == expected.Target && actual.Source == expected.Source && actual.RW != expected.ReadOnly {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// Disabling must also remove older mounts on the next stopped-container start.
	if plan.revision == "" {
		for _, actual := range info.Mounts {
			if actual.Destination == toolstore.PublicMount || actual.Destination == toolstore.CacheMount {
				return false
			}
		}
	}
	return true
}
func (m *Manager) initToolCache(ctx context.Context, cid string, plan toolMountPlan) error {
	if plan.revision == "" {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	_, err := m.ExecCommand(bounded, cid, []string{"/usr/bin/python3", "-I", "-c", toolStorageInit, plan.platform})
	if err != nil {
		return fmt.Errorf("prepare private tool cache: %w", err)
	}
	return nil
}
