package collaboration

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"agentbox/internal/safefs"
)

type GitEntry struct {
	Path, OID, Mode string
	Size            int64
}

// ParseGitTree consumes `git ls-tree -rlz --full-tree <commit>` as untrusted
// bytes. Gitlinks, unsafe paths, oversized trees and ambiguous entries fail.
func ParseGitTree(raw []byte, limits SnapshotLimits) (map[string]GitEntry, error) {
	if len(raw) > 32<<20 || limits.MaxEntries < 1 || limits.MaxBytes < 1 {
		return nil, ErrInvalid
	}
	out := map[string]GitEntry{}
	total := int64(0)
	if len(raw) > 0 && raw[len(raw)-1] != 0 {
		return nil, ErrInvalid
	}
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, ErrInvalid
		}
		meta := strings.Fields(string(parts[0]))
		name := string(parts[1])
		if len(meta) != 4 || meta[1] != "blob" || (meta[0] != "100644" && meta[0] != "100755" && meta[0] != "120000") || !validProjectPath(name) {
			return nil, ErrInvalid
		}
		if len(meta[2]) != 40 && len(meta[2]) != 64 {
			return nil, ErrInvalid
		}
		if _, err := hex.DecodeString(meta[2]); err != nil {
			return nil, ErrInvalid
		}
		size, err := strconv.ParseInt(meta[3], 10, 64)
		if err != nil || size < 0 || size > limits.MaxBytes-total {
			return nil, ErrInvalid
		}
		total += size
		if _, exists := out[name]; exists || len(out) >= limits.MaxEntries {
			return nil, ErrInvalid
		}
		out[name] = GitEntry{Path: name, OID: meta[2], Mode: meta[0], Size: size}
	}
	if len(out) == 0 {
		return nil, errors.New("empty repository")
	}
	return out, nil
}

func validProjectPath(name string) bool {
	if name == "" || name == "." || len(name) > 4096 || !utf8.ValidString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\x00\\") || strings.HasPrefix(name, "../") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".git" || part == ".." {
			return false
		}
	}
	return true
}

// ImportGitArchive writes only to a fresh service-owned staging root. The
// caller removes that entire staging tree on any failure. No Git attributes,
// tar metadata, archive link or submodule can select a host path or command.
func ImportGitArchive(ctx context.Context, reader io.Reader, dest *safefs.Root, tree map[string]GitEntry, limits SnapshotLimits) error {
	expected := make(map[string]GitEntry, len(tree))
	allowedDirs := map[string]bool{}
	for k, v := range tree {
		expected[k] = v
		for parent := path.Dir(k); parent != "."; parent = path.Dir(parent) {
			allowedDirs[parent] = true
		}
	}
	tr := tar.NewReader(&snapshotReader{ctx: ctx, r: reader})
	var metadata []SnapshotEntry
	dirs := map[string]bool{}
	total := int64(0)
	count := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++
		if count > limits.MaxEntries*2+1 {
			return errors.New("archive entry limit")
		}
		// git archive emits a global PAX record containing only its commit
		// comment. It is metadata, not a file in ls-tree. Do not accept global
		// path/link overrides which could reinterpret later entries.
		if header.Typeflag == tar.TypeXGlobalHeader {
			for key, value := range header.PAXRecords {
				if key != "comment" || len(value) > 128 {
					return ErrInvalid
				}
			}
			continue
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !validProjectPath(name) {
			return ErrInvalid
		}
		if header.Typeflag == tar.TypeDir {
			if dirs[name] || !allowedDirs[name] || header.Size != 0 || header.Linkname != "" {
				return ErrInvalid
			}
			dirs[name] = true
			if err = mkdirProjectDirs(dest, name, 0755); err != nil {
				return err
			}
			metadata = append(metadata, SnapshotEntry{Path: name, Kind: "directory"})
			continue
		}
		entry, ok := expected[name]
		if !ok {
			return errors.New("unexpected archive file")
		}
		delete(expected, name)
		if err = mkdirProjectDirs(dest, path.Dir(name), 0755); err != nil {
			return err
		}
		var digest hash.Hash
		if len(entry.OID) == 40 {
			digest = sha1.New()
		} else {
			digest = sha256.New()
		}
		fmt.Fprintf(digest, "blob %d\x00", entry.Size)
		if entry.Mode == "120000" {
			if header.Typeflag != tar.TypeSymlink || int64(len(header.Linkname)) != entry.Size || path.IsAbs(header.Linkname) || strings.ContainsAny(header.Linkname, "\x00\\") {
				return ErrInvalid
			}
			digest.Write([]byte(header.Linkname))
			if hex.EncodeToString(digest.Sum(nil)) != entry.OID {
				return errors.New("archive link differs from commit")
			}
			if err = dest.Symlink(header.Linkname, name, -1, -1); err != nil {
				return err
			}
			metadata = append(metadata, SnapshotEntry{Path: name, Kind: "symlink", Target: header.Linkname})
		} else {
			if header.Typeflag != tar.TypeReg || header.Size != entry.Size || header.Size > limits.MaxBytes-total {
				return ErrInvalid
			}
			mode := os.FileMode(0644)
			if entry.Mode == "100755" {
				mode = 0755
			}
			var prefix bytes.Buffer
			capture := &prefixCapture{max: 256, buf: &prefix}
			if _, err = dest.WriteAtomic(name, io.TeeReader(tr, io.MultiWriter(digest, capture)), safefs.WriteOptions{Mode: mode, MaxBytes: entry.Size, Limit: true}); err != nil {
				return err
			}
			if hex.EncodeToString(digest.Sum(nil)) != entry.OID {
				return errors.New("archive file differs from commit")
			}
			if bytes.HasPrefix(prefix.Bytes(), []byte("version https://git-lfs.github.com/spec/v1\n")) {
				return errors.New("LFS pointer cannot be used as project input")
			}
			metadata = append(metadata, SnapshotEntry{Path: name, Kind: "file"})
		}
		total += entry.Size
	}
	if len(expected) != 0 {
		return errors.New("archive omitted committed files")
	}
	// tar EOF precedes Git's block padding and process exit. Consume bounded
	// zero padding before closing the stream, so a successful archive is not
	// mistaken for a broken pipe; concatenated payloads are rejected.
	padding := make([]byte, 4096)
	left := 1 << 20
	for {
		n, e := reader.Read(padding)
		left -= n
		if left < 0 {
			return errors.New("archive trailing data limit")
		}
		for _, b := range padding[:n] {
			if b != 0 {
				return errors.New("archive has trailing payload")
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	// Some tar producers omit explicit directory headers. Reconstruct parents
	// from committed paths without allowing a file/link to masquerade as one.
	nodes := map[string]string{}
	for _, entry := range metadata {
		nodes[entry.Path] = entry.Kind
	}
	for _, entry := range tree {
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			if kind, ok := nodes[parent]; ok {
				if kind != "directory" {
					return ErrInvalid
				}
			} else {
				nodes[parent] = "directory"
				metadata = append(metadata, SnapshotEntry{Path: parent, Kind: "directory"})
			}
		}
	}
	if err := validateSnapshotLinks(metadata); err != nil {
		return err
	}
	return dest.Sync()
}

type prefixCapture struct {
	max int
	buf *bytes.Buffer
}

func (p *prefixCapture) Write(b []byte) (int, error) {
	n := min(len(b), p.max-p.buf.Len())
	if n > 0 {
		p.buf.Write(b[:n])
	}
	return len(b), nil
}
