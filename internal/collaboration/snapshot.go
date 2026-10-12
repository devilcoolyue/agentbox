package collaboration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	"agentbox/internal/safefs"
)

type SnapshotLimits struct {
	MaxBytes   int64
	MaxEntries int
}
type SnapshotEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
}
type SnapshotManifest struct {
	Version       int             `json:"version"`
	Entries       []SnapshotEntry `json:"entries"`
	Bytes         int64           `json:"bytes"`
	metadataBytes int
}
type Snapshot struct {
	ID             string           `json:"id"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Manifest       SnapshotManifest `json:"manifest"`
}

// MaterializeSnapshot makes a writable verification copy from already verified
// immutable bytes. Destination is fresh service-owned staging, never an existing
// user workspace. Each file is hashed again as it is copied.
func MaterializeSnapshot(ctx context.Context, tree, dest *safefs.Root, snapshot Snapshot) error {
	entries, err := dest.ReadDirLimit(".", 1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("snapshot destination is not empty")
	}
	for _, e := range snapshot.Manifest.Entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !validProjectPath(e.Path) {
			return ErrInvalid
		}
		switch e.Kind {
		case "directory":
			if err = mkdirProjectDirs(dest, e.Path, os.FileMode(e.Mode)); err != nil {
				return err
			}
		case "symlink":
			if err = mkdirProjectDirs(dest, path.Dir(e.Path), 0755); err != nil {
				return err
			}
			if err = dest.Symlink(e.Target, e.Path, -1, -1); err != nil {
				return err
			}
		case "file":
			if err = mkdirProjectDirs(dest, path.Dir(e.Path), 0755); err != nil {
				return err
			}
			f, err := tree.OpenFile(e.Path)
			if err != nil {
				return err
			}
			h := sha256.New()
			info, writeErr := dest.WriteAtomic(e.Path, io.TeeReader(&snapshotReader{ctx: ctx, r: f}, h), safefs.WriteOptions{Mode: os.FileMode(e.Mode), MaxBytes: e.Size, Limit: true})
			closeErr := f.Close()
			if err = errors.Join(writeErr, closeErr); err != nil {
				return err
			}
			if info.Size() != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
				return errors.New("snapshot changed while copying")
			}
		default:
			return ErrInvalid
		}
	}
	return dest.Sync()
}

// PublishSnapshot copies a quiescent project through pinned directory handles.
// Source must have no writers; destination must be a service-owned snapshots
// root outside every mounted directory. Git metadata is omitted deliberately;
// the exported code manifest captures untracked files, modes and safe links.
// Only after recursive fsync and a no-replace directory rename is it published.
func PublishSnapshot(ctx context.Context, source, destination *safefs.Root, id string, limits SnapshotLimits) (result Snapshot, err error) {
	if !ValidID(id) || limits.MaxBytes < 1 || limits.MaxBytes > 1<<40 || limits.MaxEntries < 1 || limits.MaxEntries > 100000 {
		return result, ErrInvalid
	}
	if _, e := destination.Lstat(id); !os.IsNotExist(e) {
		if e != nil {
			return result, e
		}
		return result, errors.New("snapshot already exists")
	}
	staging := ".staging-" + rand.Text()
	if err = destination.Mkdir(staging, 0700); err != nil {
		return result, err
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, destination.RemoveAll(staging))
		}
	}()
	stage, err := destination.Sub(staging)
	if err != nil {
		return result, err
	}
	defer stage.Close()
	if err = mkdirProjectDirs(stage, "tree", 0755); err != nil {
		return result, err
	}
	tree, err := stage.Sub("tree")
	if err != nil {
		return result, err
	}
	defer tree.Close()
	manifest := SnapshotManifest{Version: 1, Entries: []SnapshotEntry{}}
	if err = copySnapshotTree(ctx, source, tree, "", 0, limits, &manifest); err != nil {
		return result, err
	}
	if err = validateSnapshotLinks(manifest.Entries); err != nil {
		return result, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return result, err
	}
	if len(raw) > 32<<20 {
		return result, errors.New("snapshot manifest exceeds limit")
	}
	if _, err = stage.WriteFile("manifest.json", raw, safefs.WriteOptions{Mode: 0444, MaxBytes: 32 << 20}); err != nil {
		return result, err
	}
	if err = stage.Sync(); err != nil {
		return result, err
	}
	if err = destination.ChmodDir(staging, 0755); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = destination.RenameTo(staging, destination, id, false); err != nil {
		return result, err
	}
	published = true
	if err = destination.Sync(); err != nil {
		return result, err
	}
	sum := sha256.Sum256(raw)
	return Snapshot{ID: id, ManifestSHA256: hex.EncodeToString(sum[:]), Manifest: manifest}, nil
}

func copySnapshotTree(ctx context.Context, source, dest *safefs.Root, prefix string, depth int, limits SnapshotLimits, m *SnapshotManifest) error {
	if depth > 64 {
		return errors.New("snapshot directory depth limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := limits.MaxEntries - len(m.Entries)
	// One extra entry permits excluding the root Git administration directory.
	entries, err := source.ReadDirLimit(".", max(1, remaining+1))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if name == ".git" {
			if prefix == "" {
				continue
			}
			return errors.New("nested repository unsupported")
		}
		rel := path.Join(prefix, name)
		if len(rel) > 4096 || len(m.Entries) >= limits.MaxEntries {
			return errors.New("snapshot entry limit")
		}
		info, err := source.Lstat(name)
		if err != nil {
			return err
		}
		m.metadataBytes += len(rel)*6 + 256
		if m.metadataBytes > 32<<20 {
			return errors.New("snapshot manifest limit")
		}
		e := SnapshotEntry{Path: rel, Mode: uint32(info.Mode().Perm())}
		switch {
		case info.IsDir():
			e.Kind = "directory"
			m.Entries = append(m.Entries, e)
			if err = mkdirProjectDirs(dest, name, 0755); err != nil {
				return err
			}
			child, err := source.Sub(name)
			if err != nil {
				return err
			}
			copy, err := dest.Sub(name)
			if err != nil {
				child.Close()
				return err
			}
			err = copySnapshotTree(ctx, child, copy, rel, depth+1, limits, m)
			child.Close()
			copy.Close()
			if err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if meta, ok := info.Sys().(*syscall.Stat_t); !ok || meta.Nlink != 1 {
				return errors.New("hard linked project file unsupported")
			}
			if info.Size() > limits.MaxBytes-m.Bytes {
				return errors.New("snapshot byte limit")
			}
			f, err := source.OpenFile(name)
			if err != nil {
				return err
			}
			opened, err := f.Stat()
			if err != nil || !os.SameFile(info, opened) {
				f.Close()
				return errors.New("project file changed while opening")
			}
			hash := sha256.New()
			mode := os.FileMode(0444)
			if info.Mode()&0111 != 0 {
				mode = 0555
			}
			written, e2 := dest.WriteAtomic(name, io.TeeReader(&snapshotReader{ctx: ctx, r: f}, hash), safefs.WriteOptions{Mode: mode, MaxBytes: limits.MaxBytes - m.Bytes, Limit: true})
			after, e3 := f.Stat()
			closeErr := f.Close()
			if err = errors.Join(e2, e3, closeErr); err != nil {
				return err
			}
			if written.Size() != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				return errors.New("project changed during snapshot")
			}
			e.Kind, e.Size, e.SHA256 = "file", written.Size(), hex.EncodeToString(hash.Sum(nil))
			m.Bytes += e.Size
			m.Entries = append(m.Entries, e)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := source.Readlink(name)
			if err != nil {
				return err
			}
			resolved := path.Clean(path.Join(prefix, target))
			if target == "" || len(target) > 4096 || path.IsAbs(target) || strings.ContainsAny(target, "\x00\\") || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return errors.New("project link escapes snapshot")
			}
			m.metadataBytes += len(target) * 6
			if m.metadataBytes > 32<<20 {
				return errors.New("snapshot manifest limit")
			}
			if err = dest.Symlink(target, name, -1, -1); err != nil {
				return err
			}
			e.Kind, e.Target = "symlink", target
			m.Entries = append(m.Entries, e)
		default:
			return errors.New("special project file unsupported")
		}
	}
	return dest.Sync()
}

// Resolve links component by component, including .. AFTER preceding symlink
// expansion. Lexical Clean alone misses paths such as deep/back/../outside
// when deep/back points at the snapshot root. No target is ever read on host.
func validateSnapshotLinks(entries []SnapshotEntry) error {
	byPath := map[string]SnapshotEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	for _, e := range entries {
		if e.Kind != "symlink" {
			continue
		}
		pending := strings.Split(e.Path, "/")
		stack := []string{}
		links := 0
		for len(pending) > 0 {
			component := pending[0]
			pending = pending[1:]
			switch component {
			case "", ".":
				continue
			case "..":
				if len(stack) == 0 {
					return errors.New("project link escapes snapshot")
				}
				stack = stack[:len(stack)-1]
				continue
			}
			name := strings.Join(append(append([]string{}, stack...), component), "/")
			target, ok := byPath[name]
			if !ok {
				return errors.New("project link target missing")
			}
			if target.Kind == "symlink" {
				links++
				if links > 40 {
					return errors.New("project link cycle or depth limit")
				}
				pending = append(strings.Split(target.Target, "/"), pending...)
			} else {
				if len(pending) > 0 && target.Kind != "directory" {
					return errors.New("project link traverses non-directory")
				}
				stack = append(stack, component)
			}
		}
	}
	return nil
}

type snapshotReader struct {
	ctx context.Context
	r   io.Reader
}

// VerifySnapshot checks stored manifest bytes against the DB's pinned digest
// and walks the actual published tree before mounting or delivering it. Missing,
// extra or changed entries fail closed; a valid manifest alone proves nothing
// about restored/copied project files.
func VerifySnapshot(ctx context.Context, root *safefs.Root, id, expected string, limits SnapshotLimits) (Snapshot, error) {
	if !ValidID(id) || len(expected) != 64 || limits.MaxEntries < 1 || limits.MaxEntries > 100000 || limits.MaxBytes < 1 || limits.MaxBytes > 1<<40 {
		return Snapshot{}, ErrInvalid
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return Snapshot{}, ErrInvalid
	}
	snapshot, err := root.Sub(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer snapshot.Close()
	raw, err := snapshot.ReadAll("manifest.json", 32<<20)
	if err != nil {
		return Snapshot{}, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != expected {
		return Snapshot{}, errors.New("snapshot manifest digest mismatch")
	}
	var m SnapshotManifest
	if Decode(raw, &m, 32<<20) != nil || m.Version != 1 || len(m.Entries) > limits.MaxEntries || m.Bytes < 0 || m.Bytes > limits.MaxBytes {
		return Snapshot{}, ErrInvalid
	}
	entries := map[string]SnapshotEntry{}
	var bytes int64
	for _, e := range m.Entries {
		if e.Path == "" || e.Path == "." || len(e.Path) > 4096 || path.Clean(e.Path) != e.Path || path.IsAbs(e.Path) || e.Path == ".." || strings.HasPrefix(e.Path, "../") || strings.ContainsAny(e.Path, "\x00\\") || e.Mode > 0777 {
			return Snapshot{}, ErrInvalid
		}
		if _, ok := entries[e.Path]; ok {
			return Snapshot{}, ErrInvalid
		}
		entries[e.Path] = e
		switch e.Kind {
		case "directory":
			if e.Size != 0 || e.SHA256 != "" || e.Target != "" {
				return Snapshot{}, ErrInvalid
			}
		case "file":
			if e.Size < 0 || e.Size > limits.MaxBytes-bytes || len(e.SHA256) != 64 || e.Target != "" {
				return Snapshot{}, ErrInvalid
			}
			if _, err := hex.DecodeString(e.SHA256); err != nil {
				return Snapshot{}, ErrInvalid
			}
			bytes += e.Size
		case "symlink":
			if e.Size != 0 || e.SHA256 != "" || e.Target == "" || len(e.Target) > 4096 || path.IsAbs(e.Target) || strings.ContainsAny(e.Target, "\x00\\") {
				return Snapshot{}, ErrInvalid
			}
		default:
			return Snapshot{}, ErrInvalid
		}
	}
	if bytes != m.Bytes {
		return Snapshot{}, ErrInvalid
	}
	if err = validateSnapshotLinks(m.Entries); err != nil {
		return Snapshot{}, err
	}
	info, err := snapshot.Lstat("tree")
	if err != nil {
		return Snapshot{}, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0755 {
		return Snapshot{}, errors.New("snapshot root directory changed")
	}
	tree, err := snapshot.Sub("tree")
	if err != nil {
		return Snapshot{}, err
	}
	defer tree.Close()
	if err = verifySnapshotTree(ctx, tree, "", 0, entries); err != nil {
		return Snapshot{}, err
	}
	if len(entries) != 0 {
		return Snapshot{}, errors.New("snapshot files missing")
	}
	return Snapshot{ID: id, ManifestSHA256: expected, Manifest: m}, nil
}

func verifySnapshotTree(ctx context.Context, root *safefs.Root, prefix string, depth int, expected map[string]SnapshotEntry) error {
	if depth > 64 {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := root.ReadDirLimit(".", max(1, len(expected)))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		key := path.Join(prefix, name)
		want, ok := expected[key]
		if !ok {
			return errors.New("unexpected snapshot entry")
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		switch want.Kind {
		case "directory":
			if !info.IsDir() || info.Mode().Perm() != 0755 {
				return errors.New("snapshot directory changed")
			}
			child, err := root.Sub(name)
			if err != nil {
				return err
			}
			err = verifySnapshotTree(ctx, child, key, depth+1, expected)
			child.Close()
			if err != nil {
				return err
			}
		case "symlink":
			if info.Mode()&os.ModeSymlink == 0 {
				return errors.New("snapshot link changed")
			}
			target, err := root.Readlink(name)
			if err != nil {
				return err
			}
			if target != want.Target {
				return errors.New("snapshot link changed")
			}
		case "file":
			mode := os.FileMode(0444)
			if want.Mode&0111 != 0 {
				mode = 0555
			}
			if !info.Mode().IsRegular() || info.Size() != want.Size || info.Mode().Perm() != mode {
				return errors.New("snapshot file metadata changed")
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
				return errors.New("snapshot file hard linked")
			}
			f, err := root.OpenFile(name)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, readErr := io.Copy(hash, io.LimitReader(&snapshotReader{ctx: ctx, r: f}, want.Size+1))
			err = errors.Join(readErr, f.Close())
			if err != nil {
				return err
			}
			if n != want.Size || hex.EncodeToString(hash.Sum(nil)) != want.SHA256 {
				return errors.New("snapshot file digest changed")
			}
		}
		delete(expected, key)
	}
	return nil
}

func (r *snapshotReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
