// Package archivex extracts uploaded code archives into a workspace and
// streams workspaces back out as zip downloads. Extraction is hardened
// against zip-slip, symlink entries and decompression bombs.
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
)

// ExtractLimitMultiplier caps total uncompressed size relative to the
// configured upload limit, guarding against decompression bombs.
const ExtractLimitMultiplier = 8

func IsArchiveName(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".zip") || strings.HasSuffix(l, ".tar.gz") || strings.HasSuffix(l, ".tgz") || strings.HasSuffix(l, ".tar")
}

// Extract unpacks the archive at src (with original filename name) into dst.
// Returns the number of files written.
func Extract(src, name, dst string, maxBytes int64) (int, error) {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".zip"):
		return extractZip(src, dst, maxBytes)
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"), strings.HasSuffix(l, ".tar"):
		return extractTar(src, dst, maxBytes, strings.HasSuffix(l, ".tar"))
	default:
		return 0, fmt.Errorf("unsupported archive type: %s", name)
	}
}

func safeJoin(root, entry string) (string, error) {
	entry = filepath.FromSlash(entry)
	if entry == "" || filepath.IsAbs(entry) || !filepath.IsLocal(entry) {
		return "", fmt.Errorf("unsafe path in archive: %q", entry)
	}
	return filepath.Join(root, entry), nil
}

func extractZip(src, dst string, maxBytes int64) (int, error) {
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
			continue // never materialize symlinks from user archives
		}
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return count, err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return count, err
			}
			continue
		}
		if !mode.IsRegular() {
			continue
		}
		total += int64(f.UncompressedSize64)
		if total > maxBytes {
			return count, fmt.Errorf("archive exceeds extraction limit (%d MB)", maxBytes>>20)
		}
		rc, err := f.Open()
		if err != nil {
			return count, err
		}
		err = writeFile(target, rc, maxBytes-total+int64(f.UncompressedSize64), mode)
		rc.Close()
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func extractTar(src, dst string, maxBytes int64, plain bool) (int, error) {
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
		switch hdr.Typeflag {
		case tar.TypeDir:
			target, err := safeJoin(dst, hdr.Name)
			if err != nil {
				return count, err
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return count, err
			}
		case tar.TypeReg:
			target, err := safeJoin(dst, hdr.Name)
			if err != nil {
				return count, err
			}
			total += hdr.Size
			if total > maxBytes {
				return count, fmt.Errorf("archive exceeds extraction limit (%d MB)", maxBytes>>20)
			}
			if err := writeFile(target, tr, hdr.Size+1, hdr.FileInfo().Mode()); err != nil {
				return count, err
			}
			count++
		default:
			// symlinks, devices etc. are dropped
		}
	}
}

func writeFile(target string, r io.Reader, limit int64, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(r, limit))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// ZipDir streams dir as a zip archive to w. Only regular files are included.
func ZipDir(w io.Writer, dir string) error {
	zw := zip.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, f)
		f.Close()
		return err
	})
	if err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// ChownTree recursively changes ownership so the container user can write.
func ChownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}
