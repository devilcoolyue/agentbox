package dockerx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

const collaborationProfileLabel = "agentbox.collaboration-profile"

// CollaborationProfile is server-owned runtime state, never an HTTP body.
// The workspace service must resolve and pin trusted mount roots and publish
// snapshots before supplying these paths. The image must have passed the
// collaboration capability gate for the chosen provider.
type CollaborationProfile struct {
	ReadOnly      bool
	RunID         string
	ParticipantID string
	Role          collaboration.Role
	ImageID       string
	Workspace     string
	Home          string
	Scratch       string
	Inputs        string
}

func (p CollaborationProfile) validate() error {
	if !collaboration.ValidID(p.RunID) || !collaboration.ValidID(p.ParticipantID) || !collaboration.ValidRole(p.Role) || !imageIDRE.MatchString(p.ImageID) {
		return errors.New("invalid collaboration profile")
	}
	paths := []string{p.Workspace, p.Home, p.Scratch, p.Inputs}
	for i, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("invalid collaboration mount")
		}
		for _, other := range paths[:i] {
			a, _ := filepath.Rel(path, other)
			b, _ := filepath.Rel(other, path)
			if filepath.IsLocal(a) || filepath.IsLocal(b) {
				return errors.New("overlapping collaboration mounts")
			}
		}
	}
	return nil
}

func (p CollaborationProfile) revision(plan toolMountPlan) string {
	raw, _ := json.Marshal(struct {
		Profile CollaborationProfile
		Tools   string
		Version int
	}{p, plan.revision, 1})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (p CollaborationProfile) mounts(plan toolMountPlan) []mount.Mount {
	return append([]mount.Mount{
		{Type: mount.TypeBind, Source: p.Workspace, Target: WorkspaceMount, ReadOnly: (p.Role == collaboration.Coordinator || p.ReadOnly)},
		{Type: mount.TypeBind, Source: p.Home, Target: ContainerHome},
		{Type: mount.TypeBind, Source: p.Scratch, Target: SharedMount},
		{Type: mount.TypeBind, Source: p.Inputs, Target: "/task-inputs", ReadOnly: true},
	}, plan.mounts...)
}

func (p CollaborationProfile) matches(info container.InspectResponse, plan toolMountPlan) bool {
	if info.Config == nil || info.Image != p.ImageID || info.Config.Labels[collaborationProfileLabel] != p.revision(plan) {
		return false
	}
	expected := p.mounts(plan)
	if len(expected) != len(info.Mounts) {
		return false
	}
	for _, want := range expected {
		found := false
		for _, got := range info.Mounts {
			if string(got.Type) == string(want.Type) && got.Source == want.Source && got.Destination == want.Target && got.RW != want.ReadOnly {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// EnsureCollaborationRunning shares normal resource/quota/browser policy but
// does not inherit ordinary /shared, writable caches or container environment.
// Unlike ordinary spaces, a running participant is never silently reused or
// killed on a name collision: the scheduler must stop and confirm the old turn.
func (m *Manager) EnsureCollaborationRunning(ctx context.Context, sess store.Session, acct config.Account, p CollaborationProfile) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if sess.ID != p.ParticipantID || sess.AccountID != acct.ID || sess.Agent != acct.Type {
		return "", errors.New("collaboration participant identity mismatch")
	}
	img, err := m.cli.ImageInspect(ctx, p.ImageID)
	if err != nil {
		return "", err
	}
	if img.Config == nil || img.Os != "linux" || len(img.Config.Volumes) > 0 || len(img.Config.OnBuild) > 0 {
		return "", errors.New("image cannot preserve collaboration mount isolation")
	}
	plan, err := m.participantToolPlan(ctx, sess.User, p.ParticipantID, p.ImageID)
	if err != nil {
		return "", err
	}
	if sess.ContainerID != "" {
		info, e := m.cli.ContainerInspect(ctx, sess.ContainerID)
		if e != nil && !client.IsErrNotFound(e) {
			return "", e
		}
		if e == nil {
			if info.Config == nil || info.Config.Labels["agentbox.session"] != sess.ID || info.Config.Labels["agentbox.collaboration"] != p.RunID || info.Config.Labels["agentbox.user"] != sess.User {
				return "", errors.New("container ownership mismatch")
			}
			if info.State == nil || info.State.Running {
				return "", errors.New("collaboration participant is still running")
			}
			// Recreate stopped participants. This also clears stale image ENV,
			// root filesystem side effects and earlier writable mount aliases.
			if e = m.removeNetwork(ctx, sess.ContainerID); e != nil {
				return "", e
			}
			if e = m.cli.ContainerRemove(ctx, sess.ContainerID, container.RemoveOptions{}); e != nil {
				return "", e
			}
		}
	}
	env := map[string]string{}
	for _, v := range img.Config.Env {
		key, _, _ := strings.Cut(v, "=")
		env[key] = ""
	}
	for _, v := range baseContainerEnv() {
		key, val, _ := strings.Cut(v, "=")
		env[key] = val
	}
	env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, v := range img.Config.Env {
		if strings.HasPrefix(v, "PATH=") {
			env["PATH"] = strings.TrimPrefix(v, "PATH=")
		}
	}
	for _, v := range plan.env {
		key, val, _ := strings.Cut(v, "=")
		env[key] = val
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+env[key])
	}
	lim := m.cfg.GetContainer()
	pids := lim.PidsLimit
	init := true
	cc := &container.Config{Image: p.ImageID, Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, WorkingDir: WorkspaceMount, User: execUser, Env: values, Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}, Labels: map[string]string{"agentbox.session": sess.ID, "agentbox.user": sess.User, "agentbox.account": acct.ID, "agentbox.collaboration": p.RunID, collaborationProfileLabel: p.revision(plan)}}
	hc := &container.HostConfig{Mounts: p.mounts(plan), NetworkMode: container.NetworkMode(lim.Network), SecurityOpt: []string{"no-new-privileges:true"}, CapDrop: []string{"ALL"}, Init: &init, Resources: container.Resources{Memory: lim.MemoryMB << 20, NanoCPUs: int64(lim.CPUs * 1e9), PidsLimit: &pids}}
	browserContainerOptions(cc, hc, img.Config.Labels)
	if m.cfg.GetStoragePolicy().Mount != "" {
		if err = quotaContainerOptions(cc, hc); err != nil {
			return "", err
		}
	}
	resp, err := m.cli.ContainerCreate(ctx, cc, hc, nil, nil, "agentbox-"+sess.ID)
	if err != nil {
		return "", fmt.Errorf("create collaboration container: %w", err)
	}
	cleanup := func(cause error) (string, error) {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return "", errors.Join(cause, m.cli.ContainerRemove(c, resp.ID, container.RemoveOptions{Force: true}))
	}
	if err = m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return cleanup(err)
	}
	actual, err := m.cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		return cleanup(err)
	}
	if !p.matches(actual, plan) {
		return cleanup(errors.New("collaboration mount verification failed"))
	}
	if err = m.initToolCache(ctx, resp.ID, plan); err != nil {
		return cleanup(err)
	}
	return resp.ID, nil
}
