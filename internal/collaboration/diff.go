package collaboration

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type FileChange struct {
	Path   string         `json:"path"`
	Status string         `json:"status"`
	Before *SnapshotEntry `json:"before,omitempty"`
	After  *SnapshotEntry `json:"after,omitempty"`
}

func SnapshotChanges(before, after SnapshotManifest) []FileChange {
	old := map[string]SnapshotEntry{}
	current := map[string]SnapshotEntry{}
	for _, e := range before.Entries {
		old[e.Path] = e
	}
	for _, e := range after.Entries {
		current[e.Path] = e
	}
	paths := map[string]bool{}
	for p := range old {
		paths[p] = true
	}
	for p := range current {
		paths[p] = true
	}
	names := []string{}
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	out := []FileChange{}
	for _, p := range names {
		a, aok := old[p]
		b, bok := current[p]
		if aok && bok && a == b {
			continue
		}
		c := FileChange{Path: p, Status: "modified"}
		if aok {
			c.Before = &a
		} else {
			c.Status = "added"
		}
		if bok {
			c.After = &b
		} else {
			c.Status = "deleted"
		}
		out = append(out, c)
	}
	return out
}

// TextDiff uses one bounded hunk with common edges removed. It never invokes
// Git or reads a path; callers supply bytes from verified immutable snapshots.
func TextDiff(path, before, after string) (string, bool) {
	if !utf8.ValidString(before) || !utf8.ValidString(after) || strings.ContainsRune(before, 0) || strings.ContainsRune(after, 0) {
		return "", true
	}
	lines := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.SplitAfter(s, "\n")
	}
	a, b := lines(before), lines(after)
	if len(a) > 0 && a[len(a)-1] == "" {
		a = a[:len(a)-1]
	}
	if len(b) > 0 && b[len(b)-1] == "" {
		b = b[:len(b)-1]
	}
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	if prefix == len(a) && prefix == len(b) {
		return "", false
	}
	start := max(0, prefix-3)
	endA := min(len(a), len(a)-suffix+3)
	endB := min(len(b), len(b)-suffix+3)
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", path, path, start+min(1, endA-start), endA-start, start+min(1, endB-start), endB-start)
	write := func(prefix string, line string) {
		out.WriteString(prefix)
		out.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
	for _, line := range a[start:prefix] {
		write(" ", line)
	}
	for _, line := range a[prefix : len(a)-suffix] {
		write("-", line)
	}
	for _, line := range b[prefix : len(b)-suffix] {
		write("+", line)
	}
	for _, line := range a[len(a)-suffix : endA] {
		write(" ", line)
	}
	return out.String(), false
}
