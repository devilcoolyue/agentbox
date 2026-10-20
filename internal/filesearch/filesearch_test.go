package filesearch

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentbox/internal/safefs"
)

func tree(t *testing.T, files map[string]string) *safefs.Root {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func paths(entries []Entry) []string {
	out := []string{}
	for _, e := range entries {
		p := e.Path
		if e.Dir {
			p += "/"
		}
		out = append(out, p)
	}
	return out
}

func indexed(ix *Index) map[string]bool {
	out := map[string]bool{}
	for _, e := range ix.entries {
		out[e.Path] = true
	}
	return out
}

func TestBuildFollowsGitignore(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":               "# build output\n/dist\n*.log\n!keep.log\ncache/\nsecret.txt \n\\#literal\n",
		"keep.log":                 "",
		"debug.log":                "",
		"#literal":                 "",
		"dist/app.js":              "",
		"src/dist/note.md":         "", // "/dist" is anchored to the root
		"src/main.go":              "",
		"src/cache":                "", // "cache/" only matches directories
		"lib/cache/blob":           "",
		"secret.txt":               "",
		"node_modules/x/index.js":  "",
		".git/HEAD":                "",
		".github/workflows/ci.yml": "",
		".agentbox-write-1":        "",
		"app/.gitignore":           "generated/\n*.tmp\n",
		"app/generated/a.ts":       "",
		"app/x.tmp":                "",
		"app/main.ts":              "",
		"other/x.tmp":              "", // app/.gitignore doesn't reach here
		"deep/a/b/c/gen/out.txt":   "",
		"deep/.gitignore":          "a/**/gen\n",
	})
	ix := Build(context.Background(), root, DefaultLimits)
	got := indexed(ix)
	for _, want := range []string{"keep.log", "src/dist/note.md", "src/main.go", "src/cache", "lib", ".github/workflows/ci.yml", "app/main.ts", "other/x.tmp", "deep/a/b/c"} {
		if !got[want] {
			t.Errorf("missing %s", want)
		}
	}
	for _, gone := range []string{"debug.log", "#literal", "dist", "dist/app.js", "lib/cache", "lib/cache/blob", "secret.txt", "node_modules", ".git", ".agentbox-write-1", "app/generated", "app/x.tmp", "deep/a/b/c/gen"} {
		if got[gone] {
			t.Errorf("%s should be left out", gone)
		}
	}
	if ix.Truncated {
		t.Fatal("small tree reported truncated")
	}
}

func TestBuildListsSymlinksWithoutFollowing(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "host.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ix := Build(context.Background(), root, DefaultLimits)
	got := indexed(ix)
	if !got["escape"] || got["escape/host.txt"] {
		t.Fatalf("symlink should be listed but not entered: %v", got)
	}
	for _, e := range ix.entries {
		if e.Path == "escape" && e.Dir {
			t.Fatal("symlink to a directory is not a folder to open")
		}
	}
}

func TestBuildLimitsKeepTheTop(t *testing.T) {
	files := map[string]string{"top.txt": "", "a/b/c/deep.txt": ""}
	for i := 0; i < 30; i++ {
		files["many/f"+string(rune('a'+i%26))+strings.Repeat("x", i/26)] = ""
	}
	root := tree(t, files)
	ix := Build(context.Background(), root, Limits{MaxEntries: 10, MaxDirEntries: 1000, MaxDepth: 32})
	got := indexed(ix)
	if !ix.Truncated || ix.Len() != 10 || !got["top.txt"] || !got["a"] || !got["many"] {
		t.Fatalf("breadth first walk should keep the top level: truncated=%v %v", ix.Truncated, got)
	}
	// Which names come first is the file system's directory order (hash order
	// on ext4), so only the count is fixed.
	ix = Build(context.Background(), root, Limits{MaxEntries: 1000, MaxDirEntries: 5, MaxDepth: 32})
	many := 0
	for path := range indexed(ix) {
		if strings.HasPrefix(path, "many/") {
			many++
		}
	}
	if !ix.Truncated || many != 5 {
		t.Fatalf("a directory past its entry limit keeps that many names and marks the index truncated: truncated=%v, %d names", ix.Truncated, many)
	}
	ix = Build(context.Background(), root, Limits{MaxEntries: 1000, MaxDirEntries: 1000, MaxDepth: 2})
	if got := indexed(ix); !got["a/b"] || got["a/b/c"] {
		t.Fatalf("depth limit: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ix := Build(ctx, root, DefaultLimits); !ix.Truncated || ix.Len() != 0 {
		t.Fatal("a cancelled build returns what it has, marked truncated")
	}
}

func TestSearchRanking(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":                "",
		"src/readme_test.go":       "",
		"src/main.go":              "",
		"src/server/main.go":       "",
		"src/server/maintain.go":   "",
		"docs/domain/main.md":      "",
		"web/src/main.ts":          "",
		"web/src/demo/MainView.ts": "",
		"cmd/agentbox/main.go":     "",
		"internal/mention.go":      "",
		"empty/":                   "",
	})
	ix := Build(context.Background(), root, DefaultLimits)

	if got := paths(ix.Search("", 10)); !reflect.DeepEqual(got, []string{"cmd/", "docs/", "empty/", "internal/", "src/", "web/", "README.md"}) {
		t.Fatalf("empty query lists the top level, folders first: %v", got)
	}
	if got := paths(ix.Search("src/", 10)); !reflect.DeepEqual(got, []string{"src/server/", "src/main.go", "src/readme_test.go"}) {
		t.Fatalf("a trailing slash lists that folder: %v", got)
	}
	if got := paths(ix.Search("WEB/SRC/", 10)); !reflect.DeepEqual(got, []string{"web/src/demo/", "web/src/main.ts"}) {
		t.Fatalf("folder listing ignores case: %v", got)
	}
	got := paths(ix.Search("main.go", 10))
	if !reflect.DeepEqual(got[:3], []string{"src/main.go", "src/server/main.go", "cmd/agentbox/main.go"}) {
		t.Fatalf("exact names first, shallow before deep: %v", got)
	}
	got = paths(ix.Search("mai", 20))
	if got[0] != "src/main.go" || indexOf(got, "src/server/maintain.go") > indexOf(got, "internal/mention.go") {
		t.Fatalf("name prefix beats a scattered match: %v", got)
	}
	if got := paths(ix.Search("readme", 3)); !reflect.DeepEqual(got, []string{"README.md", "src/readme_test.go"}) {
		t.Fatalf("matching ignores case: %v", got)
	}
	if got := paths(ix.Search("srvmain", 5)); len(got) == 0 || got[0] != "src/server/main.go" {
		t.Fatalf("characters in order match anywhere, tightest first: %v", got)
	}
	if got := paths(ix.Search("./src/server/m", 5)); !reflect.DeepEqual(got, []string{"src/server/main.go", "src/server/maintain.go"}) {
		t.Fatalf("path prefix: %v", got)
	}
	if got := ix.Search("zzzz", 5); len(got) != 0 {
		t.Fatalf("no match: %v", got)
	}
	if got := ix.Search("main", 2); len(got) != 2 {
		t.Fatalf("limit: %d", len(got))
	}
	if got := paths(ix.Search("nothere/", 5)); len(got) != 0 {
		t.Fatalf("a missing folder falls back to matching its name: %v", got)
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return len(list)
}

func TestSubsequence(t *testing.T) {
	for _, c := range []struct {
		s, q string
		want int
	}{
		{"src/server/main.go", "srvmain", 10},
		{"abc", "abc", 2},
		{"axxbxc", "abc", 5},
		{"aaab", "ab", 1},
		{"abc", "abd", -1},
		{"ab", "abc", -1},
	} {
		if got := subsequence(c.s, c.q); got != c.want {
			t.Errorf("subsequence(%q, %q) = %d, want %d", c.s, c.q, got, c.want)
		}
	}
}

func TestIgnoreRules(t *testing.T) {
	f := parseIgnore(".", "*.o\n!important.o\nbuild/\n/docs/*.pdf\n**/tmp/**\nfoo**bar\n[\n")
	for _, c := range []struct {
		path string
		dir  bool
		want bool
	}{
		{"x.o", false, true},
		{"a/b/x.o", false, true},
		{"important.o", false, false},
		{"build", true, true},
		{"build", false, false},
		{"a/build", true, true},
		{"docs/a.pdf", false, true},
		{"docs/sub/a.pdf", false, false},
		{"a/docs/a.pdf", false, false},
		{"a/tmp/x", false, true},
		{"fooxbar", false, true},
		{"[", false, false}, // a malformed glob is dropped, not fatal
	} {
		if got := ignored([]*ignoreFile{f}, c.path, c.dir); got != c.want {
			t.Errorf("%s (dir=%v): ignored=%v, want %v", c.path, c.dir, got, c.want)
		}
	}
	if parseIgnore(".", "# only comments\n\n") != nil {
		t.Fatal("an ignore file without rules adds nothing")
	}
	nested := parseIgnore("pkg", "/gen\n")
	if !ignored([]*ignoreFile{nested}, "pkg/gen", true) || ignored([]*ignoreFile{nested}, "pkg/sub/gen", true) {
		t.Fatal("an anchored pattern is relative to its own .gitignore")
	}
	// A deeper file overrides a shallower one.
	if ignored([]*ignoreFile{parseIgnore(".", "*.md\n"), parseIgnore("docs", "!*.md\n")}, "docs/a.md", false) {
		t.Fatal("deeper negation should win")
	}
}

func TestCacheReusesAndRebuilds(t *testing.T) {
	var c Cache
	builds := 0
	build := func() *Index { builds++; return &Index{Built: time.Now()} }
	a, _ := c.Get(context.Background(), "s1", time.Minute, build)
	b, _ := c.Get(context.Background(), "s1", time.Minute, build)
	if a != b || builds != 1 {
		t.Fatalf("fresh index should be reused (builds=%d)", builds)
	}
	if _, err := c.Get(context.Background(), "s1", 0, build); err != nil || builds != 2 {
		t.Fatalf("stale index should be rebuilt (builds=%d)", builds)
	}
	for i := 0; i < cacheSlots+5; i++ {
		c.Get(context.Background(), string(rune('A'+i)), time.Minute, build)
	}
	if len(c.slots) > cacheSlots {
		t.Fatalf("cache grew to %d workspaces", len(c.slots))
	}
	c.mu.Lock()
	for _, s := range c.slots {
		s.used = s.used.Add(-cacheIdle - time.Second)
	}
	c.mu.Unlock()
	c.Get(context.Background(), "fresh", time.Minute, build)
	if len(c.slots) != 1 {
		t.Fatalf("idle workspaces kept: %d", len(c.slots))
	}

	// A request waiting on another's build gives up with its own context.
	slot := c.slots["fresh"]
	slot.lock <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "fresh", 0, build); err == nil {
		t.Fatal("waiting request should stop when its context ends")
	}
	<-slot.lock
}
