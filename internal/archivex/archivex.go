// Package archivex extracts uploads and streams workspace zip downloads through
// pinned directory handles. Archive entries and existing workspace paths are
// both untrusted: lexical zip-slip checks alone do not stop symlink races.
package archivex

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"agentbox/internal/safefs"
)

const ExtractLimitMultiplier = 8

func IsArchiveName(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".zip") || strings.HasSuffix(l, ".tar.gz") || strings.HasSuffix(l, ".tgz") || strings.HasSuffix(l, ".tar")
}

// Extract accepts a service-owned staging directory as dst. Live container
// directories should be opened from data_dir and passed to ExtractRoot.
func Extract(src, name, dst string, maxBytes int64) (int, error) {
	root, err := safefs.Open(dst)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	return ExtractRoot(src, name, root, maxBytes)
}

func ExtractRoot(src, name string, root *safefs.Root, maxBytes int64) (int, error) {
	if maxBytes <= 0 {
		return 0, fmt.Errorf("invalid extraction limit")
	}
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".zip"):
		return extractZip(src, root, maxBytes)
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"), strings.HasSuffix(l, ".tar"):
		return extractTar(src, root, maxBytes, strings.HasSuffix(l, ".tar"))
	default:
		return 0, fmt.Errorf("unsupported archive type: %s", name)
	}
}

func archivePath(entry string) (string, error) {
	entry = filepath.FromSlash(entry)
	if entry == "" || !filepath.IsLocal(entry) {
		return "", fmt.Errorf("unsafe path in archive: %q", entry)
	}
	return filepath.Clean(entry), nil
}

func extractZip(src string, root *safefs.Root, maxBytes int64) (int, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	var total int64
	count := 0
	for _, f := range r.File {
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			continue
		}
		name, err := archivePath(f.Name)
		if err != nil {
			return count, err
		}
		if f.FileInfo().IsDir() {
			if err := root.MkdirAll(name, 0o755); err != nil {
				return count, err
			}
			continue
		}
		if !mode.IsRegular() {
			continue
		}
		if f.UncompressedSize64 > uint64(maxBytes-total) {
			return count, fmt.Errorf("archive exceeds extraction limit (%d MB)", maxBytes>>20)
		}
		rc, err := f.Open()
		if err != nil {
			return count, err
		}
		n, err := writeFile(root, name, rc, maxBytes-total, mode)
		rc.Close()
		if err != nil {
			return count, err
		}
		total += n
		count++
	}
	return count, nil
}

func extractTar(src string, root *safefs.Root, maxBytes int64, plain bool) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var tr *tar.Reader
	if plain {
		tr = tar.NewReader(f)
	} else {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		tr = tar.NewReader(gz)
	}
	var total int64
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeReg {
			continue
		}
		name, err := archivePath(hdr.Name)
		if err != nil {
			return count, err
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(name, 0o755); err != nil {
				return count, err
			}
			continue
		}
		if hdr.Size < 0 || hdr.Size > maxBytes-total {
			return count, fmt.Errorf("archive exceeds extraction limit (%d MB)", maxBytes>>20)
		}
		n, err := writeFile(root, name, tr, maxBytes-total, hdr.FileInfo().Mode())
		if err != nil {
			return count, err
		}
		total += n
		count++
	}
}

func writeFile(root *safefs.Root, name string, r io.Reader, limit int64, mode fs.FileMode) (int64, error) {
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return 0, err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	info, err := root.WriteAtomic(name, r, safefs.WriteOptions{Mode: perm, MaxBytes: limit, Limit: true})
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func ZipDir(w io.Writer, dir string) error {
	root, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return ZipRoot(w, root)
}

func ZipRoot(w io.Writer, root *safefs.Root) error {
	zw := zip.NewWriter(w)
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := root.OpenFile(name)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(name)
		hdr.Method = zip.Deflate
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.CopyN(fw, f, info.Size())
		return err
	})
	if err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

func ChownTree(dir string, uid, gid int) error {
	root, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return ChownRoot(root, uid, gid)
}

func ChownRoot(root *safefs.Root, uid, gid int) error {
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return root.Chown(name, uid, gid)
	})
}
