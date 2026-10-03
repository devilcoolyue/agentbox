package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"time"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
	"golang.org/x/sys/unix"
)

// Read-only preparation: sync remains zero until leases and writes are ready.
func (s *Server) handleClientManifest(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.clientManifestOnce.Do(func() { s.clientManifestSlots = make(chan struct{}, 2) })
	select {
	case s.clientManifestSlots <- struct{}{}:
		defer func() { <-s.clientManifestSlots }()
	default:
		writeErr(w, http.StatusTooManyRequests, "清单扫描繁忙，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var manifest syncproto.Manifest
	var project store.ClientProject
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
		var err error
		project, err = s.store.ClientProject(current.ID, r.URL.Query().Get("project"))
		if err != nil {
			return err
		}
		if !store.ClientProjectPath(project.Path) {
			return syncproto.ErrInvalid
		}
		// Reuse the trusted data_dir anchor, never EvalSymlinks untrusted paths.
		workspace, err := s.openDataDir(s.workspaceDir(current))
		if err != nil {
			return err
		}
		defer workspace.Close()
		root, err := workspace.Sub(project.Path)
		if err != nil {
			return err
		}
		defer root.Close()
		identity, err := root.Lstat(".")
		if err != nil {
			return err
		}
		manifest, err = scanClientManifest(ctx, root)
		if err != nil {
			return err
		}
		fresh, err := workspace.Sub(project.Path)
		if err != nil {
			return err
		}
		defer fresh.Close()
		actual, err := fresh.Lstat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(identity, actual) {
			return syncproto.ErrChanged
		}
		return nil
	})
	if err != nil {
		writeManifestError(w, err)
		return
	}
	issues, err := syncfs.CheckNames(manifest, syncfs.NamePolicy{Windows: true, MaxPathUnits: 200})
	if err != nil {
		writeManifestError(w, err)
		return
	}
	digest, err := manifest.Digest()
	if err != nil {
		writeManifestError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"manifest": manifest, "digest": digest, "windows_issues": issues, "project": project.ID, "project_revision": project.Revision, "project_path": project.Path})
}

func writeManifestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, syncproto.ErrChanged):
		writeErr(w, http.StatusConflict, "扫描期间目录或文件已改变，请重试")
	case errors.Is(err, syncproto.ErrLimit):
		writeErr(w, http.StatusRequestEntityTooLarge, "目录超过扫描数量、文件大小或总量限制")
	case errors.Is(err, syncproto.ErrInvalid), errors.Is(err, safefs.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, "目录包含不支持的文件、链接或忽略规则")
	case os.IsNotExist(err), os.IsPermission(err):
		writeErr(w, http.StatusConflict, "项目目录或文件已不可访问")
	default:
		writeClientError(w, err)
	}
}
func clientIgnoreFile(ctx context.Context, root *safefs.Root) ([]byte, bool, error) {
	info, err := root.Lstat(".agentboxignore")
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, syncproto.ErrInvalid
	}
	if info.Size() > 64<<10 {
		return nil, false, syncproto.ErrLimit
	}
	var data boundedSnapshot
	data.remaining = 64 << 10
	if _, err = copyManifestFile(ctx, root, ".agentboxignore", info, &data); err != nil {
		return nil, false, err
	}
	return data.buffer.Bytes(), true, nil
}
func scanClientManifest(ctx context.Context, root *safefs.Root) (syncproto.Manifest, error) {
	text, exists, err := clientIgnoreFile(ctx, root)
	if err != nil {
		return syncproto.Manifest{}, err
	}
	rules, err := syncproto.ParseRules(string(text))
	if err != nil {
		return syncproto.Manifest{}, err
	}
	manifest := syncproto.Manifest{Version: syncproto.Version, RulesHash: rules.Hash(), Entries: map[string]syncproto.Entry{}}
	var total int64
	visited := 0
	var scan func(*safefs.Root, string, int) error
	scan = func(dir *safefs.Root, prefix string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 128 {
			return syncproto.ErrLimit
		}
		before, err := dir.Lstat(".")
		if err != nil {
			return err
		}
		opened, err := dir.FS().Open(".")
		if err != nil {
			return err
		}
		defer opened.Close()
		reader, ok := opened.(fs.ReadDirFile)
		if !ok {
			return syncproto.ErrInvalid
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, readErr := reader.ReadDir(256)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			for _, entry := range entries {
				visited++
				if visited > syncproto.DefaultMaxEntries {
					return syncproto.ErrLimit
				}
				name := path.Join(prefix, entry.Name())
				if rules.Ignored(name) {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if !syncproto.ValidPath(name) {
					return syncproto.ErrInvalid
				}
				if len(manifest.Entries) >= syncproto.DefaultMaxEntries {
					return syncproto.ErrLimit
				}
				info, err := dir.Lstat(entry.Name())
				if err != nil {
					return err
				}
				if info.IsDir() {
					child, err := dir.Sub(entry.Name())
					if err != nil {
						return err
					}
					manifest.Entries[name] = syncproto.Entry{Kind: "directory"}
					err = scan(child, name, depth+1)
					child.Close()
					if err != nil {
						return err
					}
				} else if info.Mode().IsRegular() {
					value, err := hashManifestFile(ctx, dir, entry.Name(), info)
					if err != nil {
						return err
					}
					total += value.Size
					if total > syncproto.DefaultMaxScanBytes {
						return syncproto.ErrLimit
					}
					manifest.Entries[name] = value
				} else {
					return syncproto.ErrInvalid
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
		after, err := dir.Lstat(".")
		if err != nil {
			return err
		}
		if !before.ModTime().Equal(after.ModTime()) {
			return syncproto.ErrChanged
		}
		return nil
	}
	if err = scan(root, "", 0); err != nil {
		return syncproto.Manifest{}, err
	}
	current, present, err := clientIgnoreFile(ctx, root)
	if err != nil {
		return syncproto.Manifest{}, err
	}
	if exists != present || !bytes.Equal(text, current) {
		return syncproto.Manifest{}, syncproto.ErrChanged
	}
	if err = manifest.Validate(); err != nil {
		return syncproto.Manifest{}, err
	}
	return manifest, nil
}
func hashManifestFile(ctx context.Context, root *safefs.Root, name string, before os.FileInfo) (syncproto.Entry, error) {
	return copyManifestFile(ctx, root, name, before, io.Discard)
}

// Copy and hash the same opened inode. Callers must keep copied bytes private
// until the complete read and all metadata checks have succeeded.
func copyManifestFile(ctx context.Context, root *safefs.Root, name string, before os.FileInfo, dst io.Writer) (syncproto.Entry, error) {
	if before.Size() > syncproto.DefaultMaxFileBytes {
		return syncproto.Entry{}, syncproto.ErrLimit
	}
	f, err := root.OpenFile(name)
	if err != nil {
		return syncproto.Entry{}, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(int(f.Fd()), &st); err != nil {
		return syncproto.Entry{}, err
	}
	if st.Nlink != 1 {
		return syncproto.Entry{}, syncproto.ErrInvalid
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, hasher), io.LimitReader(manifestReader{ctx, f}, syncproto.DefaultMaxFileBytes+1))
	if err != nil {
		return syncproto.Entry{}, err
	}
	if n > syncproto.DefaultMaxFileBytes {
		return syncproto.Entry{}, syncproto.ErrLimit
	}
	after, err := f.Stat()
	if err != nil {
		return syncproto.Entry{}, err
	}
	if err = unix.Fstat(int(f.Fd()), &st); err != nil {
		return syncproto.Entry{}, err
	}
	if st.Nlink != 1 {
		return syncproto.Entry{}, syncproto.ErrInvalid
	}
	current, err := root.Lstat(name)
	if err != nil {
		return syncproto.Entry{}, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(after, current) || n != before.Size() || n != after.Size() || n != current.Size() || !before.ModTime().Equal(after.ModTime()) || !after.ModTime().Equal(current.ModTime()) || before.Mode() != after.Mode() || after.Mode() != current.Mode() {
		return syncproto.Entry{}, syncproto.ErrChanged
	}
	return syncproto.Entry{Kind: "file", Size: n, Hash: hex.EncodeToString(hasher.Sum(nil)), Executable: after.Mode().Perm()&0111 != 0}, nil
}

type manifestReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r manifestReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
