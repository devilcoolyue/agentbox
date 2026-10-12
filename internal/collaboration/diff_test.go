package collaboration

import (
	"strings"
	"testing"
)

func TestSnapshotDiffIncludesModesLinksAndDeletions(t *testing.T) {
	a := SnapshotManifest{Entries: []SnapshotEntry{{Path: "old", Kind: "file"}, {Path: "tool", Kind: "file", Mode: 0644}, {Path: "link", Kind: "symlink", Target: "old"}}}
	b := SnapshotManifest{Entries: []SnapshotEntry{{Path: "new", Kind: "file"}, {Path: "tool", Kind: "file", Mode: 0755}, {Path: "link", Kind: "symlink", Target: "new"}}}
	changes := SnapshotChanges(a, b)
	if len(changes) != 4 || changes[0].Path != "link" || changes[1].Status != "added" || changes[2].Status != "deleted" || changes[3].Before.Mode != 0644 {
		t.Fatal(changes)
	}
	diff, binary := TextDiff("file", "same\nold\ntail\n", "same\nnew\ntail\n")
	if binary || !strings.Contains(diff, "-old\n+new\n") {
		t.Fatal(diff)
	}
	diff, binary = TextDiff("new", "", "new")
	if binary || !strings.Contains(diff, "-0,0 +1,1") || !strings.Contains(diff, "No newline") {
		t.Fatal(diff)
	}
	if _, binary = TextDiff("bin", "\x00", ""); !binary {
		t.Fatal("binary treated as text")
	}
	for _, p := range []string{"dist/a", "build", "coverage/x"} {
		if !VerificationOutputPath(p) {
			t.Fatal(p)
		}
	}
	for _, p := range []string{"source.ts", "builder/file", "src/dist/x"} {
		if VerificationOutputPath(p) {
			t.Fatal(p)
		}
	}
}
