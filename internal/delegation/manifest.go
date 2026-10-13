package delegation

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"sort"
	"time"
)

// The manifest is the platform's own evidence of what a worker changed. It
// compares metadata only (size, mtime, mode, link target); it never reads
// contents and never follows links. Bulky generated trees are skipped.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, ".cache": true}

const (
	manifestMaxEntries = 50000
	manifestDeadline   = 10 * time.Second
	maxChangedPaths    = 200
)

type entry struct {
	size, mtime int64
	mode        fs.FileMode
}

// Manifest is a bounded snapshot; Complete is false when a limit cut it short.
type Manifest struct {
	files    map[string]entry
	Complete bool
}

// Snapshot walks fsys. lstat must not follow symlinks (safefs.Root.FS does not).
func Snapshot(ctx context.Context, fsys fs.FS) Manifest {
	m := Manifest{files: map[string]entry{}, Complete: true}
	deadline := time.Now().Add(manifestDeadline)
	errStop := errors.New("stop")
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			m.Complete = false
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) || len(m.files) >= manifestMaxEntries {
			m.Complete = false
			return errStop
		}
		if d.IsDir() {
			if p != "." && skipDirs[path.Base(p)] {
				return fs.SkipDir
			}
			return nil
		}
		info, e := d.Info()
		if e != nil {
			m.Complete = false
			return nil
		}
		m.files[p] = entry{size: info.Size(), mtime: info.ModTime().UnixNano(), mode: info.Mode()}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		m.Complete = false
	}
	return m
}

// Changes lists what differs between two snapshots.
type Changes struct {
	Known     bool     `json:"known"`
	Added     []string `json:"added"`
	Modified  []string `json:"modified"`
	Deleted   []string `json:"deleted"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated,omitempty"`
}

func Diff(before, after Manifest) Changes {
	c := Changes{Known: before.Complete && after.Complete, Added: []string{}, Modified: []string{}, Deleted: []string{}}
	if !c.Known {
		// A cut-off walk cannot tell "deleted" from "not reached"; report
		// nothing rather than a misleading list.
		return c
	}
	add := func(list *[]string, p string) {
		c.Total++
		if len(c.Added)+len(c.Modified)+len(c.Deleted) >= maxChangedPaths {
			c.Truncated = true
			return
		}
		*list = append(*list, p)
	}
	keys := make([]string, 0, len(after.files))
	for p := range after.files {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		old, ok := before.files[p]
		switch {
		case !ok:
			add(&c.Added, p)
		case old != after.files[p]:
			add(&c.Modified, p)
		}
	}
	gone := []string{}
	for p := range before.files {
		if _, ok := after.files[p]; !ok {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	for _, p := range gone {
		add(&c.Deleted, p)
	}
	return c
}
