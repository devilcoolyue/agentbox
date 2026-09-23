package agent

import (
	"agentbox/internal/safefs"
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
	home, err := openHome(homeDir)
	if err != nil {
		return err
	}
	defer home.Close()
	merged := map[string]templateEntry{}
	for _, dir := range templateDirs {
		if dir == "" {
			continue
		}
		root, err := openHome(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		defer root.Close()
		if err := collectTemplate(root, ".", merged); err != nil {
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
		dst := filepath.FromSlash(rel)
		var err error
		switch {
		case e.info.IsDir():
			err = ensureTemplateDir(home, dst, e.info.Mode().Perm(), uid, gid)
		case e.info.Mode()&os.ModeSymlink != 0:
			err = seedTemplateLink(e.root, e.src, home, dst, uid, gid)
		default:
			err = seedTemplateFile(e.root, e.src, home, dst, e.info, uid, gid)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// templateEntry is one resolved source path for a home-relative slash path.
type templateEntry struct {
	root *safefs.Root
	src  string
	info os.FileInfo // lstat: symlinks are described, not resolved
}

func collectTemplate(root *safefs.Root, rel string, out map[string]templateEntry) error {
	entries, err := root.ReadDir(rel)
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
		out[childRel] = templateEntry{root: root, src: filepath.FromSlash(childRel), info: info}
		if info.IsDir() {
			if err := collectTemplate(root, childRel, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureTemplateDir(root *safefs.Root, name string, perm os.FileMode, uid, gid int) error {
	switch fi, err := root.Lstat(name); {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("home template: %s exists and is not a directory", name)
	case !os.IsNotExist(err):
		return err
	}
	if err := root.MkdirAll(name, perm); err != nil {
		return err
	}
	if err := root.ChmodDir(name, perm); err != nil {
		return err
	}
	return root.Chown(name, uid, gid)
}

func seedTemplateLink(src *safefs.Root, source string, dst *safefs.Root, target string, uid, gid int) error {
	link, err := src.Readlink(source)
	if err != nil {
		return err
	}
	if current, err := dst.Readlink(target); err == nil {
		if current == link {
			return nil
		}
		if err := dst.RemoveAll(target); err != nil {
			return err
		}
	} else if _, err := dst.Lstat(target); err == nil {
		return nil
	}
	return dst.Symlink(link, target, uid, gid)
}

func seedTemplateFile(src *safefs.Root, source string, dst *safefs.Root, target string, si os.FileInfo, uid, gid int) error {
	if di, err := dst.Lstat(target); err == nil {
		if !di.Mode().IsRegular() || !si.ModTime().After(di.ModTime()) {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := src.OpenFile(source)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = dst.WriteAtomic(target, f, safefs.WriteOptions{Mode: si.Mode().Perm(), Chown: true, UID: uid, GID: gid, ModTime: si.ModTime()})
	return err
}
