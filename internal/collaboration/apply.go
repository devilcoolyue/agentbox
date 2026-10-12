package collaboration

import (
	"agentbox/internal/safefs"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"time"
)

// CloneWorkspace stages a complete stopped workspace, including ignored files
// and Git metadata. Symlinks are copied as links and never traversed. Limits
// fail before publication; source is untouched throughout staging.
func CloneWorkspace(ctx context.Context, src, dst *safefs.Root, maxBytes int64, maxEntries int) error {
	count := 0
	var used int64
	var walk func(string) error
	walk = func(dir string) error {
		items, err := src.ReadDirLimit(dir, maxEntries-count)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err = ctx.Err(); err != nil {
				return err
			}
			count++
			if count > maxEntries {
				return ErrInvalid
			}
			name := path.Join(dir, item.Name())
			info, e := src.Lstat(name)
			if e != nil {
				return e
			}
			switch {
			case info.IsDir():
				if e = dst.Mkdir(name, info.Mode().Perm()); e == nil {
					e = dst.SetDirMetadata(name, info.Mode().Perm(), 1000, 1000, info.ModTime())
				}
				if e == nil {
					e = walk(name)
					if e == nil {
						child, err := dst.Sub(name)
						if err != nil {
							return err
						}
						e = child.Sync()
						e = errors.Join(e, child.Close())
					}
				}
			case info.Mode().IsRegular():
				used += info.Size()
				if used > maxBytes {
					return ErrInvalid
				}
				var f *os.File
				f, e = src.OpenFile(name)
				if e == nil {
					_, e = dst.WriteAtomic(name, f, safefs.WriteOptions{Mode: info.Mode().Perm(), Chown: true, UID: 1000, GID: 1000, ModTime: info.ModTime(), MaxBytes: info.Size(), Limit: true})
					e = errors.Join(e, f.Close())
				}
			case info.Mode()&os.ModeSymlink != 0:
				var target string
				target, e = src.Readlink(name)
				if e == nil {
					e = dst.Symlink(target, name, 1000, 1000)
				}
			default:
				return ErrInvalid
			}
			if e != nil {
				return e
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return err
	}
	return dst.Sync()
}

// ApplySnapshot edits only the staged copy. Every original entry must match
// its baseline, so ignored files colliding with additions cannot be replaced.
func ApplySnapshot(ctx context.Context, project, tree *safefs.Root, before, after Snapshot) error {
	changes := SnapshotChanges(before.Manifest, after.Manifest)
	for _, c := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.Before != nil {
			if err := matchesEntry(project, *c.Before); err != nil {
				return err
			}
		} else {
			if _, err := project.Lstat(c.Path); !os.IsNotExist(err) {
				return errors.New("result collides with an existing path")
			}
		}
	}
	// Remove children before parents; never recursively delete ignored files.
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if c.Before == nil {
			continue
		}
		if c.Before.Kind == "directory" && c.After != nil && c.After.Kind == "directory" {
			continue
		}
		if err := project.Remove(c.Path); err != nil {
			return err
		}
	}
	for _, c := range changes {
		if c.After == nil {
			continue
		}
		e := *c.After
		switch e.Kind {
		case "directory":
			if c.Before == nil || c.Before.Kind != "directory" {
				if err := project.Mkdir(e.Path, os.FileMode(e.Mode)); err != nil {
					return err
				}
			}
			if err := project.SetDirMetadata(e.Path, os.FileMode(e.Mode), 1000, 1000, time.Time{}); err != nil {
				return err
			}
		case "symlink":
			if err := project.Symlink(e.Target, e.Path, 1000, 1000); err != nil {
				return err
			}
		case "file":
			f, err := tree.OpenFile(e.Path)
			if err != nil {
				return err
			}
			_, err = project.WriteAtomic(e.Path, f, safefs.WriteOptions{Mode: os.FileMode(e.Mode), Chown: true, UID: 1000, GID: 1000, MaxBytes: e.Size, Limit: true})
			err = errors.Join(err, f.Close())
			if err != nil {
				return err
			}
		default:
			return ErrInvalid
		}
	}
	for _, e := range after.Manifest.Entries {
		if err := matchesEntry(project, e); err != nil {
			return err
		}
	}
	return project.Sync()
}
func matchesEntry(root *safefs.Root, e SnapshotEntry) error {
	info, err := root.Lstat(e.Path)
	if err != nil {
		return err
	}
	switch e.Kind {
	case "directory":
		if !info.IsDir() {
			return ErrInvalid
		}
	case "symlink":
		if info.Mode()&os.ModeSymlink == 0 {
			return ErrInvalid
		}
		target, err := root.Readlink(e.Path)
		if err != nil || target != e.Target {
			return ErrInvalid
		}
	case "file":
		if !info.Mode().IsRegular() || info.Size() != e.Size || uint32(info.Mode().Perm()) != e.Mode {
			return ErrInvalid
		}
		f, err := root.OpenFile(e.Path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, io.LimitReader(f, e.Size+1))
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
