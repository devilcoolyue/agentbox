package storagepolicy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"agentbox/internal/safefs"
)

var ownerRE = regexp.MustCompile(`^[a-f0-9]{64}$`)
var platformRE = regexp.MustCompile(`^linux-(amd64|arm64)-[a-z0-9]+-[a-z0-9.]+$`)

// CleanCaches is called only under the workspace start gate with every known
// container stopped. No module cache, browser, profile, or SDK is eligible.
func CleanCaches(ctx context.Context, tools, owner string, target int64, age time.Duration) ([]string, error) {
	if !ownerRE.MatchString(owner) {
		return nil, errors.New("invalid cache owner")
	}
	root, err := safefs.Open(tools)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	cache, err := root.Sub("private/" + owner + "/cache")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer cache.Close()
	entries, err := fs.ReadDir(cache.FS(), ".")
	if err != nil {
		return nil, err
	}
	type candidate struct {
		path   string
		size   int64
		newest time.Time
	}
	var candidates []candidate
	var total int64
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || !platformRE.MatchString(entry.Name()) {
			continue
		}
		for _, kind := range []string{"go-build", "npm/_cacache"} {
			name := entry.Name() + "/" + kind
			dir, err := cache.Sub(name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			c := candidate{path: name}
			err = fs.WalkDir(dir.FS(), ".", func(path string, d fs.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if e = ctx.Err(); e != nil {
					return e
				}
				count++
				if count > 200000 {
					return errors.New("cache scan incomplete")
				}
				info, e := d.Info()
				if e != nil {
					return e
				}
				if info.ModTime().After(c.newest) {
					c.newest = info.ModTime()
				}
				if info.Mode().IsRegular() {
					c.size += info.Size()
				}
				return nil
			})
			dir.Close()
			if err != nil {
				return nil, err
			}
			total += c.size
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].newest.Before(candidates[j].newest) })
	var removed []string
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		expired := age > 0 && time.Since(c.newest) > age
		over := target > 0 && total > target
		if !expired && !over {
			continue
		}
		if err := cache.RemoveAll(filepath.FromSlash(c.path)); err != nil {
			return removed, err
		}
		removed = append(removed, c.path)
		total -= c.size
	}
	return removed, nil
}
