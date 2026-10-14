// Package filesearch indexes a workspace's file names for the chat composer's
// "@" mentions. Claude Code inlines the content of an @path itself, so the
// composer only has to find and spell the path; nothing but names is read here
// (and each directory's .gitignore, to leave out what the CLI's own suggestions
// leave out).
package filesearch

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"agentbox/internal/safefs"
)

// Limits bound one index build. A workspace holding a dependency tree that no
// .gitignore covers would otherwise make every keystroke walk it.
type Limits struct {
	MaxEntries    int // names kept in the index
	MaxDirEntries int // names read from one directory
	MaxDepth      int
}

var DefaultLimits = Limits{MaxEntries: 50_000, MaxDirEntries: 10_000, MaxDepth: 32}

// Never listed, with or without a .gitignore: VCS metadata, dependency trees
// and caches nobody means to mention, plus the server's own temporary names.
var skipNames = map[string]bool{".git": true, ".hg": true, ".svn": true, "node_modules": true, "__pycache__": true, ".venv": true, ".DS_Store": true}

type Entry struct {
	Path  string // slash separated, relative to the workspace
	Dir   bool
	lower string
	base  int // where the last path element starts
	depth int
}

type Index struct {
	entries []Entry
	// Truncated reports that a limit or the deadline stopped the walk: the
	// shallowest names are there, deeper ones may be missing.
	Truncated bool
	Built     time.Time
}

func (ix *Index) Len() int { return len(ix.entries) }

type pending struct {
	rel    string
	depth  int
	ignore []*ignoreFile
}

// Build walks breadth first, so a walk cut short by a limit or ctx still holds
// the top of the tree. Symlinks are listed but never followed; a directory
// that vanishes or turns into a link mid-walk is skipped.
func Build(ctx context.Context, root *safefs.Root, lim Limits) *Index {
	ix := &Index{}
	queue := []pending{{rel: ".", depth: 0}}
	for len(queue) > 0 {
		if ctx.Err() != nil {
			ix.Truncated = true
			break
		}
		cur := queue[0]
		queue = queue[1:]
		names, ignore, full := readDir(root, cur, lim.MaxDirEntries)
		if !full {
			ix.Truncated = true
		}
		for _, n := range names {
			rel := n.name
			if cur.rel != "." {
				rel = cur.rel + "/" + n.name
			}
			if skipNames[n.name] || strings.HasPrefix(n.name, ".agentbox-") || ignored(ignore, rel, n.dir) {
				continue
			}
			if len(ix.entries) >= lim.MaxEntries {
				ix.Truncated = true
				queue = nil
				break
			}
			ix.entries = append(ix.entries, newEntry(rel, n.dir, cur.depth))
			if n.dir && cur.depth+1 < lim.MaxDepth {
				queue = append(queue, pending{rel: rel, depth: cur.depth + 1, ignore: ignore})
			}
		}
	}
	ix.Built = time.Now()
	return ix
}

func newEntry(rel string, dir bool, depth int) Entry {
	return Entry{Path: rel, Dir: dir, lower: strings.ToLower(rel), base: strings.LastIndexByte(rel, '/') + 1, depth: depth}
}

type dirName struct {
	name string
	dir  bool
}

// readDir returns the directory's listable names sorted, the ignore files in
// force for them, and whether every name was read.
func readDir(root *safefs.Root, cur pending, max int) ([]dirName, []*ignoreFile, bool) {
	dir, err := root.Sub(cur.rel)
	if err != nil {
		return nil, nil, true
	}
	defer dir.Close()
	ignore := cur.ignore
	if raw, err := dir.ReadAll(".gitignore", maxIgnoreBytes); err == nil {
		if f := parseIgnore(cur.rel, string(raw)); f != nil {
			// Copy: sibling directories share the parent's slice.
			ignore = append(append([]*ignoreFile(nil), cur.ignore...), f)
		}
	}
	opened, err := dir.FS().Open(".")
	if err != nil {
		return nil, ignore, true
	}
	defer opened.Close()
	reader, ok := opened.(fs.ReadDirFile)
	if !ok {
		return nil, ignore, true
	}
	var out []dirName
	full := true
	for {
		// Entries of a handle opened in a root are typed through that root too.
		batch, err := reader.ReadDir(256)
		for _, e := range batch {
			if len(out) >= max {
				full = false
				break
			}
			switch t := e.Type(); {
			case t.IsDir():
				out = append(out, dirName{e.Name(), true})
			case t.IsRegular(), t&fs.ModeSymlink != 0:
				out = append(out, dirName{e.Name(), false})
			}
		}
		if !full || err != nil {
			if err != nil && !errors.Is(err, io.EOF) {
				full = false
			}
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, ignore, full
}

// Search ranks like a file picker: exact name, name prefix, name substring,
// path substring, then the query's characters in order anywhere in the path.
// Shallower and shorter paths win ties. An empty query lists the top level;
// a query ending in "/" lists that directory first.
func (ix *Index) Search(query string, limit int) []Entry {
	q := strings.TrimPrefix(strings.ToLower(query), "./")
	if q == "" {
		return ix.children("", limit)
	}
	if strings.HasSuffix(q, "/") {
		if out := ix.children(q, limit); len(out) > 0 {
			return out
		}
		q = strings.TrimRight(q, "/")
		if q == "" {
			return ix.children("", limit)
		}
	}
	type hit struct {
		e          *Entry
		tier, span int
	}
	var hits []hit
	for i := range ix.entries {
		e := &ix.entries[i]
		name := e.lower[e.base:]
		tier, span := -1, 0
		switch {
		case name == q:
			tier = 0
		case strings.HasPrefix(name, q):
			tier = 1
		case strings.Contains(name, q), strings.HasPrefix(e.lower, q):
			tier = 2
		case strings.Contains(e.lower, q):
			tier = 3
		default:
			if span = subsequence(e.lower, q); span >= 0 {
				tier = 4
			}
		}
		if tier >= 0 {
			hits = append(hits, hit{e, tier, span})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.span != b.span {
			return a.span < b.span
		}
		if a.e.depth != b.e.depth {
			return a.e.depth < b.e.depth
		}
		if len(a.e.Path) != len(b.e.Path) {
			return len(a.e.Path) < len(b.e.Path)
		}
		return a.e.Path < b.e.Path
	})
	out := make([]Entry, 0, min(limit, len(hits)))
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, *h.e)
	}
	return out
}

// children lists the entries directly inside dir ("" or "a/b/"), folders first.
func (ix *Index) children(dir string, limit int) []Entry {
	var out []Entry
	for _, e := range ix.entries {
		if e.base == len(dir) && strings.HasPrefix(e.lower, dir) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Path < out[j].Path
	})
	return out[:min(limit, len(out))]
}

// subsequence returns the width of the narrowest window, ending where q's
// characters first all occur in order in s, or -1 when they don't.
func subsequence(s, q string) int {
	end, j := -1, 0
	for i := 0; i < len(s); i++ {
		if s[i] == q[j] {
			if j++; j == len(q) {
				end = i
				break
			}
		}
	}
	if end < 0 {
		return -1
	}
	start := end
	for i, k := end, len(q)-1; k >= 0; i-- {
		if s[i] == q[k] {
			start, k = i, k-1
		}
	}
	return end - start
}

// A pattern list from one .gitignore. Git's rules, minus global excludes and
// .git/info/exclude: last match wins, "!" re-includes, a trailing "/" only
// matches directories, a pattern with an inner or leading "/" is anchored to
// the file's directory and one without matches a name at any depth.
type ignoreFile struct {
	dir   string // workspace-relative directory holding the file, "." for the root
	rules []ignoreRule
}

type ignoreRule struct {
	parts   []string
	negate  bool
	dirOnly bool
}

const (
	maxIgnoreBytes = 64 << 10
	maxIgnoreRules = 1000
)

func parseIgnore(dir, text string) *ignoreFile {
	f := &ignoreFile{dir: dir}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(strings.TrimSuffix(line, "\r"), " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		if strings.HasPrefix(line, "!") {
			r.negate, line = true, line[1:]
		} else if strings.HasPrefix(line, `\`) {
			line = line[1:] // escaped leading "#" or "!"
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimRight(line, "/")
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimLeft(line, "/")
		if line == "" {
			continue
		}
		valid := true
		for _, part := range strings.Split(line, "/") {
			if part != "**" && strings.Contains(part, "**") {
				part = strings.ReplaceAll(part, "**", "*")
			}
			if _, err := path.Match(part, ""); err != nil {
				valid = false
				break
			}
			r.parts = append(r.parts, part)
		}
		if !valid {
			continue
		}
		if !anchored {
			r.parts = append([]string{"**"}, r.parts...)
		}
		f.rules = append(f.rules, r)
		if len(f.rules) >= maxIgnoreRules {
			break
		}
	}
	if len(f.rules) == 0 {
		return nil
	}
	return f
}

func ignored(files []*ignoreFile, rel string, dir bool) bool {
	out := false
	for _, f := range files {
		sub := rel
		if f.dir != "." {
			sub = strings.TrimPrefix(rel, f.dir+"/")
		}
		parts := strings.Split(sub, "/")
		for _, r := range f.rules {
			if (!r.dirOnly || dir) && matchParts(r.parts, parts) {
				out = !r.negate
			}
		}
	}
	return out
}

// matchParts matches path elements against pattern elements where "**" stands
// for any number of elements. Dynamic programming keeps repeated "**" linear.
func matchParts(pattern, parts []string) bool {
	row := make([]bool, len(parts)+1)
	row[0] = true
	for _, segment := range pattern {
		next := make([]bool, len(parts)+1)
		if segment == "**" {
			next[0] = row[0]
			for j := 1; j <= len(parts); j++ {
				next[j] = row[j] || next[j-1]
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				matched, _ := path.Match(segment, parts[j-1])
				next[j] = row[j-1] && matched
			}
		}
		row = next
	}
	return row[len(parts)]
}
