// Package safefs confines operations to pinned directory handles. Open's base
// is administrator-controlled; every untrusted component beneath it must be
// accessed through Root, never by joining it back into an absolute OS path.
package safefs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var ErrInvalid = errors.New("invalid file operation")

type Root struct{ root *os.Root }

// Open accepts a trusted anchor, such as data_dir or a service-owned staging
// directory. Use Sub for paths that contain container-controlled components.
func Open(base string) (*Root, error) {
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	return &Root{r}, nil
}

func (r *Root) Close() error { return r.root.Close() }

func (r *Root) Readlink(name string) (string, error) {
	p, leaf, err := r.parent(name)
	if err != nil {
		return "", err
	}
	defer p.Close()
	return p.root.Readlink(leaf)
}

// Symlink copies a link verbatim; operations through this package still refuse
// to follow it. Useful for home templates pointing to container-only /shared.
func (r *Root) Symlink(target, name string, uid, gid int) error {
	p, leaf, err := r.parent(name)
	if err != nil {
		return err
	}
	defer p.Close()
	if err := p.root.Symlink(target, leaf); err != nil {
		return err
	}
	return p.root.Lchown(leaf, uid, gid)
}

func (r *Root) ChmodDir(name string, mode os.FileMode) error {
	d, err := r.Sub(name)
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := d.root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}

func clean(name string, allowRoot bool) (string, error) {
	name = filepath.Clean(filepath.FromSlash(name))
	if !filepath.IsLocal(name) || (!allowRoot && name == ".") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: invalid path", ErrInvalid)
	}
	return name, nil
}

// Sub pins each component separately. Root.OpenRoot confines link resolution;
// comparing inode identity also rejects a directory swapped for an in-root
// symlink between Lstat and OpenRoot. Once returned, renaming the directory
// doesn't retarget the handle.
func (r *Root) Sub(name string) (*Root, error) {
	name, err := clean(name, true)
	if err != nil {
		return nil, err
	}
	cur, err := r.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if name == "." {
		return &Root{cur}, nil
	}
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		info, err := cur.Lstat(part)
		if err != nil {
			cur.Close()
			return nil, err
		}
		if !info.IsDir() {
			cur.Close()
			return nil, fmt.Errorf("%w: not a directory or is a symlink", ErrInvalid)
		}
		next, err := cur.OpenRoot(part)
		cur.Close()
		if err != nil {
			return nil, err
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			next.Close()
			return nil, fmt.Errorf("%w: directory changed during access", ErrInvalid)
		}
		cur = next
	}
	return &Root{cur}, nil
}

func (r *Root) parent(name string) (*Root, string, error) {
	name, err := clean(name, false)
	if err != nil {
		return nil, "", err
	}
	p, err := r.Sub(filepath.Dir(name))
	return p, filepath.Base(name), err
}

func (r *Root) Lstat(name string) (os.FileInfo, error) {
	if name == "" || name == "." {
		return r.root.Stat(".")
	}
	p, leaf, err := r.parent(name)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.root.Lstat(leaf)
}

// OpenFile returns only a regular file. O_NONBLOCK prevents FIFO swaps from
// blocking before fstat can reject them; O_NOFOLLOW protects the final component.
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
		return nil, fmt.Errorf("%w: not a regular file", ErrInvalid)
	}
	f, err := p.root.OpenFile(leaf, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: not a regular file", ErrInvalid)
	}
	return f, nil
}

func (r *Root) ReadFile(name string, limit int64) ([]byte, error) {
	f, err := r.OpenFile(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// ReadAll rejects oversized files rather than handing parsers a truncated
// configuration which they could then persist back over the complete file.
func (r *Root) ReadAll(name string, maxBytes int64) ([]byte, error) {
	raw, err := r.ReadFile(name, maxBytes+1)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, errors.New("file exceeds size limit")
	}
	return raw, nil
}

func (r *Root) ReadDir(name string) ([]fs.DirEntry, error) {
	dir, err := r.Sub(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := dir.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		// File.ReadDir's deferred Info may resolve the old absolute path. Take
		// metadata through the pinned root instead and return an immutable copy.
		info, err := dir.root.Lstat(e.Name())
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, fs.FileInfoToDirEntry(info))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (r *Root) Mkdir(name string, mode os.FileMode) error {
	p, leaf, err := r.parent(name)
	if err != nil {
		return err
	}
	defer p.Close()
	return p.root.Mkdir(leaf, mode)
}

func (r *Root) MkdirAll(name string, mode os.FileMode) error {
	name, err := clean(name, true)
	if err != nil {
		return err
	}
	if name == "." {
		return nil
	}
	prefix := ""
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		if err := r.Mkdir(prefix, mode); err != nil && !os.IsExist(err) {
			return err
		}
		d, err := r.Sub(prefix)
		if err != nil {
			return err
		}
		d.Close()
	}
	return nil
}

func (r *Root) RemoveAll(name string) error {
	p, leaf, err := r.parent(name)
	if err != nil {
		return err
	}
	defer p.Close()
	return p.root.RemoveAll(leaf)
}

func (r *Root) Clear() error {
	entries, err := r.ReadDir(".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := r.RemoveAll(e.Name()); err != nil {
			return err
		}
	}
	return nil
}

type WriteOptions struct {
	Mode            os.FileMode
	Chown           bool
	UID, GID        int
	BestEffortChown bool
	ModTime         time.Time
	// Zero means the caller already bounded its reader.
	MaxBytes int64
	Limit    bool // enforce MaxBytes even when the remaining budget is zero
}

// WriteAtomic never truncates an existing inode (which may be hard-linked).
// The random O_EXCL temporary file, metadata updates and final rename all use
// the same pinned parent. Failed writes leave the old destination intact.
func (r *Root) WriteAtomic(name string, src io.Reader, opts WriteOptions) (os.FileInfo, error) {
	p, leaf, err := r.parent(name)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	if info, err := p.root.Lstat(leaf); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: not a regular file", ErrInvalid)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	tmp := ".agentbox-write-" + rand.Text()
	f, err := p.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer p.root.Remove(tmp)
	reader := src
	if opts.Limit || opts.MaxBytes > 0 {
		reader = io.LimitReader(src, opts.MaxBytes+1)
	}
	n, err := io.Copy(f, reader)
	if err != nil {
		return nil, err
	}
	if (opts.Limit || opts.MaxBytes > 0) && n > opts.MaxBytes {
		return nil, errors.New("file exceeds size limit")
	}
	if opts.Chown {
		if err := f.Chown(opts.UID, opts.GID); err != nil && !opts.BestEffortChown {
			return nil, err
		}
	}
	if err := f.Chmod(opts.Mode.Perm()); err != nil {
		return nil, err
	}
	if !opts.ModTime.IsZero() {
		if err := setFileTime(f, opts.ModTime); err != nil {
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := p.root.Rename(tmp, leaf); err != nil {
		return nil, err
	}
	return info, nil
}

func (r *Root) WriteFile(name string, data []byte, opts WriteOptions) (os.FileInfo, error) {
	return r.WriteAtomic(name, bytes.NewReader(data), opts)
}

// Chown uses an opened file/directory, not Root.Chown's path-based Unix syscall.
func (r *Root) Chown(name string, uid, gid int) error {
	info, err := r.Lstat(name)
	if err != nil {
		return err
	}
	var f *os.File
	if info.IsDir() {
		d, err := r.Sub(name)
		if err != nil {
			return err
		}
		defer d.Close()
		f, err = d.root.Open(".")
		if err != nil {
			return err
		}
	} else {
		f, err = r.OpenFile(name)
		if err != nil {
			return err
		}
	}
	defer f.Close()
	return f.Chown(uid, gid)
}

func setFileTime(f *os.File, t time.Time) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	err = conn.Control(func(fd uintptr) {
		v := unix.NsecToTimeval(t.UnixNano())
		callErr = unix.Futimes(int(fd), []unix.Timeval{v, v})
	})
	if err != nil {
		return err
	}
	return callErr
}

// RenameTo supports cross-scope moves without reopening either parent by name.
func (r *Root) RenameTo(name string, dst *Root, target string, replace bool) error {
	srcParent, srcLeaf, err := r.parent(name)
	if err != nil {
		return err
	}
	defer srcParent.Close()
	dstParent, dstLeaf, err := dst.parent(target)
	if err != nil {
		return err
	}
	defer dstParent.Close()
	sf, err := srcParent.root.Open(".")
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := dstParent.root.Open(".")
	if err != nil {
		return err
	}
	defer df.Close()
	return renameAt(sf, srcLeaf, df, dstLeaf, replace)
}

// FS exposes confined, no-symlink traversal to fs.WalkDir and similar helpers.
func (r *Root) FS() fs.FS { return rootFS{r} }

type rootFS struct{ r *Root }

func (f rootFS) Stat(name string) (fs.FileInfo, error)      { return f.r.Lstat(name) }
func (f rootFS) ReadDir(name string) ([]fs.DirEntry, error) { return f.r.ReadDir(name) }
func (f rootFS) Open(name string) (fs.File, error) {
	info, err := f.r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return f.r.OpenFile(name)
	}
	d, err := f.r.Sub(name)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.root.Open(".")
}
