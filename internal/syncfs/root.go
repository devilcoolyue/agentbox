// Package syncfs is the portable client filesystem boundary. It deliberately
// does not import the server's Unix-only safefs package.
package syncfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"agentbox/internal/syncproto"
)

var ErrUnsafe = errors.New("unsupported file, link, or directory")
var ErrRootChanged = errors.New("local directory is missing, moved, or replaced")
var ErrUnsupportedVolume = errors.New("local volume is unsupported or could not be verified for sync")

type volumeGuard func(*os.Root) error

type Root struct {
	root     *os.Root
	absolute string
	identity os.FileInfo
	volume   volumeGuard
}

// Open never creates a missing mapping directory. Pin each path component so
// symlink/junction traversal cannot cross into a different mapped tree.
func Open(directory string) (*Root, error) {
	return OpenContext(context.Background(), directory)
}

// OpenContext ties native volume queries to the caller lifecycle. A canceled
// desktop request must reap its helper before the sidecar finishes.
func OpenContext(ctx context.Context, directory string) (*Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	guard, err := checkVolume(ctx, abs)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return openRoot(abs, guard)
}

// openRoot can reuse only a guard captured from an already approved, pinned
// volume. It never infers approval from a device name or a global cache.
func openRoot(abs string, guard volumeGuard) (*Root, error) {
	anchor := filepath.VolumeName(abs) + string(filepath.Separator)
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	current := &Root{root: root}
	for _, part := range strings.Split(strings.TrimPrefix(abs, anchor), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		next, err := current.sub(part)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	info, err := current.root.Stat(".")
	if err != nil {
		current.Close()
		return nil, err
	}
	if guard != nil {
		if err := guard(current.root); err != nil {
			current.Close()
			return nil, err
		}
	}
	current.volume = guard
	current.absolute = abs
	current.identity = info
	return current, nil
}

func (r *Root) Close() error { return r.root.Close() }
func (r *Root) CheckIdentity() error {
	// Reopen with the same confined traversal. A renamed pin can still be read,
	// but must never be treated as the current mapping after an external move.
	if r.absolute == "" {
		return nil
	}
	var current *Root
	var err error
	if r.volume != nil {
		if err := r.volume(r.root); err != nil {
			return ErrRootChanged
		}
		// macOS: recheck the exact pinned mount without spawning diskutil in
		// per-file writes. A different mount fails before identity comparison.
		current, err = openRoot(r.absolute, r.volume)
	} else {
		current, err = Open(r.absolute)
	}
	if err != nil {
		return ErrRootChanged
	}
	defer current.Close()
	if !os.SameFile(r.identity, current.identity) {
		return ErrRootChanged
	}
	return nil
}

func (r *Root) sub(name string) (*Root, error) {
	if name == "." {
		next, err := r.root.OpenRoot(".")
		if err != nil {
			return nil, err
		}
		if r.volume != nil {
			if err := r.volume(next); err != nil {
				next.Close()
				return nil, err
			}
		}
		return &Root{root: next, volume: r.volume}, nil
	}
	if !syncproto.ValidPath(name) {
		return nil, ErrUnsafe
	}
	cur, err := r.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(name, "/") {
		info, err := cur.Lstat(part)
		if err != nil {
			cur.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			cur.Close()
			return nil, ErrUnsafe
		}
		next, err := cur.OpenRoot(part)
		cur.Close()
		if err != nil {
			return nil, err
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			next.Close()
			return nil, ErrUnsafe
		}
		if r.volume != nil {
			if err := r.volume(next); err != nil {
				next.Close()
				return nil, err
			}
		}
		cur = next
	}
	return &Root{root: cur, volume: r.volume}, nil
}

func (r *Root) parent(name string) (*Root, string, error) {
	if !syncproto.ValidPath(name) {
		return nil, "", ErrUnsafe
	}
	p, err := r.sub(path.Dir(name))
	return p, path.Base(name), err
}

func (r *Root) OpenFile(name string) (*os.File, error) {
	p, leaf, err := r.parent(name)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	info, err := p.root.Lstat(leaf)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrUnsafe
	}
	f, err := openRegular(p.root, leaf)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		f.Close()
		return nil, ErrUnsafe
	}
	if err = checkSingleLink(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

type Limits struct {
	MaxEntries   int
	MaxFileBytes int64
	MaxScanBytes int64
}

func (l Limits) defaults() Limits {
	if l.MaxEntries <= 0 || l.MaxEntries > syncproto.DefaultMaxEntries {
		l.MaxEntries = syncproto.DefaultMaxEntries
	}
	if l.MaxFileBytes <= 0 || l.MaxFileBytes > syncproto.DefaultMaxFileBytes {
		l.MaxFileBytes = syncproto.DefaultMaxFileBytes
	}
	if l.MaxScanBytes <= 0 {
		l.MaxScanBytes = syncproto.DefaultMaxScanBytes
	}
	return l
}

// Scan hashes bytes without newline conversion. Errors return no partial tree.
// Stat/hash/stat detects ordinary concurrent writes; it is not a filesystem
// snapshot and cannot exclude a writer deliberately preserving all metadata.
func (r *Root) Scan(ctx context.Context, rules syncproto.Rules, limits Limits) (syncproto.Manifest, error) {
	return r.ScanWithProgress(ctx, rules, limits, nil)
}

// ScanWithProgress reports successfully hashed files, not a complete snapshot.
// The observer must not block or modify the tree; errors still return no tree.
func (r *Root) ScanWithProgress(ctx context.Context, rules syncproto.Rules, limits Limits, observe func(string, int, int64)) (syncproto.Manifest, error) {
	if err := r.CheckIdentity(); err != nil {
		return syncproto.Manifest{}, err
	}
	limits = limits.defaults()
	m := syncproto.Manifest{Version: syncproto.Version, RulesHash: rules.Hash(), Entries: map[string]syncproto.Entry{}}
	var bytes int64
	files := 0
	visited := 0
	var walk func(string, int) error
	walk = func(prefix string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 128 {
			return syncproto.ErrLimit
		}
		dir, err := r.sub(prefix)
		if err != nil {
			return err
		}
		defer dir.Close()
		before, err := dir.root.Stat(".")
		if err != nil {
			return err
		}
		f, err := dir.root.Open(".")
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := f.ReadDir(256)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			for _, entry := range entries {
				visited++
				if visited > limits.MaxEntries {
					return syncproto.ErrLimit
				}
				name := entry.Name()
				if prefix != "." {
					name = prefix + "/" + name
				}
				if rules.Ignored(name) {
					continue
				}
				if !syncproto.ValidPath(name) {
					return fmt.Errorf("%w: path %q", ErrUnsafe, name)
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(m.Entries) >= limits.MaxEntries {
					return syncproto.ErrLimit
				}
				info, err := dir.root.Lstat(entry.Name())
				if err != nil {
					return err
				}
				if info.IsDir() && info.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
					m.Entries[name] = syncproto.Entry{Kind: "directory"}
					if err = walk(name, depth+1); err != nil {
						return err
					}
				} else if info.Mode().IsRegular() {
					value, err := r.hash(ctx, name, limits.MaxFileBytes)
					if err != nil {
						return err
					}
					bytes += value.Size
					if bytes > limits.MaxScanBytes {
						return syncproto.ErrLimit
					}
					m.Entries[name] = value
					files++
					if observe != nil {
						observe(name, files, bytes)
					}
				} else {
					return fmt.Errorf("%w: %q", ErrUnsafe, name)
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
		after, err := dir.root.Stat(".")
		if err != nil {
			return err
		}
		if !before.ModTime().Equal(after.ModTime()) {
			return syncproto.ErrChanged
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return syncproto.Manifest{}, err
	}
	if err := r.CheckIdentity(); err != nil {
		return syncproto.Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return syncproto.Manifest{}, err
	}
	return m, nil
}

func (r *Root) hash(ctx context.Context, name string, limit int64) (syncproto.Entry, error) {
	f, err := r.OpenFile(name)
	if err != nil {
		return syncproto.Entry{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return syncproto.Entry{}, err
	}
	if before.Size() > limit {
		return syncproto.Entry{}, syncproto.ErrLimit
	}
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(contextReader{ctx, f}, limit+1))
	if err != nil {
		return syncproto.Entry{}, err
	}
	if n > limit {
		return syncproto.Entry{}, syncproto.ErrLimit
	}
	after, err := f.Stat()
	if err != nil {
		return syncproto.Entry{}, err
	}
	if n != before.Size() || n != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		return syncproto.Entry{}, syncproto.ErrChanged
	}
	// Verify the name still refers to the inode whose bytes were hashed.
	current, err := r.OpenFile(name)
	if err != nil {
		return syncproto.Entry{}, err
	}
	defer current.Close()
	info, err := current.Stat()
	if err != nil {
		return syncproto.Entry{}, err
	}
	if !os.SameFile(after, info) || !after.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() {
		return syncproto.Entry{}, syncproto.ErrChanged
	}
	return syncproto.Entry{Kind: "file", Hash: hex.EncodeToString(hasher.Sum(nil)), Size: n, Executable: after.Mode().Perm()&0111 != 0}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Names produces stable previews independent of filesystem enumeration order.
func Names(m syncproto.Manifest) []string {
	out := make([]string, 0, len(m.Entries))
	for name := range m.Entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
