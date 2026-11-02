package agent

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// builtinTemplate is the platform's own home template layer: tools and skills
// that ship with this binary (today abox-remote and its Claude skill). It is
// layered underneath the server and user templates, so an operator or user can
// override any file by putting their own copy there.
//
//go:embed all:builtin
var builtinTemplate embed.FS

// BuiltinTemplateTime is the mtime stamped on every built-in file. Templates
// win over a session copy only when newer, so bumping it (together with
// builtinTemplateDigest, which TestBuiltinTemplateDigest checks) is what pushes
// changed files out to existing sessions; a file the user edited in a session
// after that moment is kept.
var BuiltinTemplateTime = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

const builtinTemplateDigest = "f086ac288f9c749b204378e22e0d2d814917a6650e7a687c6c13d3fe939f544f"

// BuiltinTemplateDigest hashes the embedded tree (paths and contents).
func BuiltinTemplateDigest() string {
	h := sha256.New()
	var files []string
	_ = fs.WalkDir(builtinTemplate, "builtin", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	sort.Strings(files)
	for _, p := range files {
		b, _ := builtinTemplate.ReadFile(p)
		h.Write([]byte(p + "\x00"))
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// WriteBuiltinTemplate materializes the built-in layer at dir, replacing what
// was there, so SeedHomeTemplate can read it like any other template. The tree
// is written beside dir and renamed into place: a session starting meanwhile
// sees the old tree, the new one or (for an instant) none, never half of one.
func WriteBuiltinTemplate(dir string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".builtin-template-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	err = fs.WalkDir(builtinTemplate, "builtin", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "builtin"), "/")
		dst := filepath.Join(tmp, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := builtinTemplate.ReadFile(p)
		if err != nil {
			return err
		}
		// embed drops file modes; everything under a bin directory is a tool.
		mode := os.FileMode(0o644)
		if path.Base(path.Dir(p)) == "bin" {
			mode = 0o755
		}
		if err := os.WriteFile(dst, b, mode); err != nil {
			return err
		}
		return os.Chtimes(dst, BuiltinTemplateTime, BuiltinTemplateTime)
	})
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	old := dir + ".old"
	_ = os.RemoveAll(old)
	if err := os.Rename(dir, old); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	return os.RemoveAll(old)
}
