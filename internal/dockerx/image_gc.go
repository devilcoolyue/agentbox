package dockerx

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

var legacyCLITag = regexp.MustCompile(`^agentbox-agent:cli-[0-9]+$`)
var managedCLITag = regexp.MustCompile(`^agentbox-agent:(cli-[0-9]+|base-[a-f0-9]{64})$`)

func runtimeConfig(raw json.RawMessage) []byte {
	var cfg map[string]json.RawMessage
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	// Legacy builders record the parent reference in Config.Image. It is build
	// metadata (not the container command/config) and necessarily changes when
	// rebuilding from a fixed base. The OCI runtime config omits this field.
	delete(cfg, "Image")
	var labels map[string]string
	if json.Unmarshal(cfg["Labels"], &labels) != nil {
		return nil
	}
	delete(labels, "agentbox.claude-code")
	delete(labels, "agentbox.codex")
	cfg["Labels"], _ = json.Marshal(labels)
	// Empty USER is Docker's default root, which our builder writes explicitly.
	var user string
	_ = json.Unmarshal(cfg["User"], &user)
	if user == "" {
		cfg["User"] = json.RawMessage(`"root"`)
	}
	out, _ := json.Marshal(cfg)
	return out
}

// LegacyCLIImages adopts only exclusively updater-tagged ancestors of the
// source, with identical runtime metadata except CLI versions. No global prune.
func (m *Manager) LegacyCLIImages(ctx context.Context, source string) ([]string, error) {
	src, raw, err := m.rawCLIImage(ctx, source)
	if err != nil {
		return nil, err
	}
	all, err := m.cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, item := range all {
		if len(item.RepoTags) == 0 {
			continue
		}
		owned := true
		for _, tag := range item.RepoTags {
			if !legacyCLITag.MatchString(tag) {
				owned = false
			}
		}
		if !owned {
			continue
		}
		old, cfg, e := m.rawCLIImage(ctx, item.ID)
		if e != nil {
			return nil, e
		}
		layers := old.RootFS.Layers
		if old.Architecture != src.Architecture || old.Variant != src.Variant || len(layers) == 0 || len(layers) > len(src.RootFS.Layers) || !slices.Equal(layers, src.RootFS.Layers[:len(layers)]) {
			continue
		}
		if sameJSON(runtimeConfig(raw), runtimeConfig(cfg)) {
			ids = append(ids, old.ID)
		}
	}
	return ids, nil
}

// RemoveCLIImage never removes containers, volumes or an image with foreign
// tags. Docker's no-force delete is a second guard against newly created refs.
func (m *Manager) RemoveCLIImage(ctx context.Context, id string, protected map[string]bool) (bool, error) {
	if protected[id] {
		return false, nil
	}
	info, err := m.cli.ImageInspect(ctx, id)
	if client.IsErrNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, tag := range info.RepoTags {
		if !managedCLITag.MatchString(tag) {
			return false, nil
		}
	}
	all, err := m.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return false, err
	}
	for _, c := range all {
		if c.ImageID == id {
			return false, nil
		}
	}
	// Deletion by ID, without force, refuses multiple tags and dependent images.
	// Refuse rather than sequentially untagging an image another caller may use.
	_, err = m.cli.ImageRemove(ctx, id, image.RemoveOptions{Force: false, PruneChildren: false})
	if errdefs.IsConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Classic Docker builds have untagged intermediate parents. Remove only this
	// exact parent chain, stopping at any protected/tagged/container-used node.
	// No implicit prune: even an untagged configured rollback ID remains pinned.
	parent := info.Parent
	for n := 0; parent != "" && n < 256; n++ {
		if protected[parent] {
			break
		}
		p, e := m.cli.ImageInspect(ctx, parent)
		if client.IsErrNotFound(e) {
			break
		}
		if e != nil {
			return true, e
		}
		if len(p.RepoTags) > 0 {
			break
		}
		_, e = m.cli.ImageRemove(ctx, parent, image.RemoveOptions{Force: false, PruneChildren: false})
		if errdefs.IsConflict(e) {
			break
		}
		if e != nil {
			return true, e
		}
		parent = p.Parent
	}
	return true, nil
}

// Report only changed field names: ENV and labels may contain private values.
func runtimeConfigDifferences(a, b json.RawMessage) string {
	var x, y map[string]json.RawMessage
	_ = json.Unmarshal(runtimeConfig(a), &x)
	_ = json.Unmarshal(runtimeConfig(b), &y)
	var keys []string
	for k, v := range x {
		if !sameJSON(v, y[k]) {
			keys = append(keys, k)
		}
	}
	for k := range y {
		if _, ok := x[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}
