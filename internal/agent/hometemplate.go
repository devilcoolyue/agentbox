package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SeedHomeTemplate overlays operator-managed skeleton directories onto a
// session home. Every session starts from a brand-new empty home, so without a
// template anything installed by hand — Claude Code skills under
// .claude/skills/, user-scope MCP servers in .claude.json, shell rc files —
// has to be reinstalled in each new session.
//
// templateDirs are layered in order, later ones winning per path: the
// server-wide template first, then the per-user one. The layering is resolved
// across the whole set *before* anything is written, because the write rule
// below compares against the session copy — applying the layers one after
// another would let an older user template lose to the global one it is
// supposed to override.
//
// The overlay runs on every session start rather than only at creation, so an
// updated template also reaches sessions that already exist. Per file the newer
// mtime wins, the same convergence rule credSync uses on credentials: a file
// edited inside the container is kept, a file updated in the template is pushed
// out. The flip side is that deleting a templated file inside a session doesn't
// stick — the next start puts it back.
//
// Symlinks are recreated as symlinks and never followed, so a template can
// point bulky content at /shared instead of duplicating it per session. Missing
// template directories are not an error: the feature is opt-in by creating one.
//
// Callers must seed templates BEFORE SeedCredentials so that a stray credential
// file in a template can never win over the account pool.
func SeedHomeTemplate(homeDir string, uid, gid int, templateDirs ...string) error {
	merged := map[string]templateEntry{}
	for _, dir := range templateDirs {
		if dir == "" {
			continue
		}
		fi, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("home template %s is not a directory", dir)
		}
		if err := collectTemplate(dir, "", merged); err != nil {
			return err
		}
	}

	rels := make([]string, 0, len(merged))
	for rel := range merged {
		rels = append(rels, rel)
	}
	// Lexical order puts every directory ahead of its children (an ancestor is
	// a strict prefix), so a parent always exists by the time we write into it.
	sort.Strings(rels)

	for _, rel := range rels {
		e := merged[rel]
		dst := filepath.Join(homeDir, filepath.FromSlash(rel))
		var err error
		switch {
		case e.info.IsDir():
			err = ensureTemplateDir(dst, e.info.Mode().Perm(), uid, gid)
		case e.info.Mode()&os.ModeSymlink != 0:
			err = seedTemplateLink(e.src, dst, uid, gid)
		default:
			err = seedTemplateFile(e.src, dst, e.info, uid, gid)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// templateEntry is one resolved source path for a home-relative slash path.
type templateEntry struct {
	src  string
	info os.FileInfo // lstat: symlinks are described, not resolved
}

func collectTemplate(root, rel string, out map[string]templateEntry) error {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return err
		}
		isLink := info.Mode()&os.ModeSymlink != 0
		if !info.IsDir() && !isLink && !info.Mode().IsRegular() {
			continue // sockets / devices / fifos: nothing sensible to seed
		}
		childRel := path.Join(rel, e.Name())
		// A later template replacing a directory with a file (or a link)
		// invalidates the children collected from the earlier one.
		if prev, ok := out[childRel]; ok && prev.info.IsDir() && !info.IsDir() {
			for k := range out {
				if strings.HasPrefix(k, childRel+"/") {
					delete(out, k)
				}
			}
		}
		out[childRel] = templateEntry{src: filepath.Join(root, filepath.FromSlash(childRel)), info: info}
		if info.IsDir() {
			if err := collectTemplate(root, childRel, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureTemplateDir(path string, perm os.FileMode, uid, gid int) error {
	switch fi, err := os.Lstat(path); {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		// Someone put a file (or symlink) where the template wants a directory;
		// clobbering it would be worse than skipping the subtree.
		return fmt.Errorf("home template: %s exists and is not a directory", path)
	case !os.IsNotExist(err):
		return err
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	if err := os.Chmod(path, perm); err != nil { // MkdirAll applied the umask
		return err
	}
	return os.Chown(path, uid, gid)
}

// seedTemplateLink mirrors a symlink verbatim. mtime comparison is meaningless
// for links, so the template simply owns the link target.
func seedTemplateLink(src, dst string, uid, gid int) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if cur, err := os.Readlink(dst); err == nil {
		if cur == target {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	} else if _, err := os.Lstat(dst); err == nil {
		return nil // a real file or directory claimed the name in-session; leave it
	}
	if err := os.Symlink(target, dst); err != nil {
		return err
	}
	return os.Lchown(dst, uid, gid)
}

func seedTemplateFile(src, dst string, si os.FileInfo, uid, gid int) error {
	if di, err := os.Lstat(dst); err == nil {
		if !di.Mode().IsRegular() || !si.ModTime().After(di.ModTime()) {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := copyFile(src, dst, uid, gid); err != nil {
		return err
	}
	// Mode carries the executable bit that hooks and statusline scripts need.
	if err := os.Chmod(dst, si.Mode().Perm()); err != nil {
		return err
	}
	// Align mtime with the source so the comparison above stays stable across
	// starts instead of recopying every time.
	return os.Chtimes(dst, si.ModTime(), si.ModTime())
}
