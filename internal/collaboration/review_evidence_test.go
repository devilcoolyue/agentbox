package collaboration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/safefs"
)

func evidenceSnapshot(t *testing.T, storage *safefs.Root, id string, files map[string]string) Snapshot {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	limits := SnapshotLimits{MaxBytes: 16 << 20, MaxEntries: 1000}
	snapshot, err := PublishSnapshot(t.Context(), root, storage, id, limits)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifySnapshot(t.Context(), storage, snapshot.ID, snapshot.ManifestSHA256, limits)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func TestReviewEvidenceCapturesChangesAndExplicitOmissions(t *testing.T) {
	root, err := safefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	a := evidenceSnapshot(t, root, strings.Repeat("a", 32), map[string]string{"delete.txt": "gone\n", "edit.txt": "before\n", "image.bin": "\x00a", "large.txt": strings.Repeat("a", 256<<10+1), ".git/config": "never in prompt"})
	b := evidenceSnapshot(t, root, strings.Repeat("b", 32), map[string]string{"add.txt": "new\n", "edit.txt": "after\n", "image.bin": "\x00b", "large.txt": strings.Repeat("b", 256<<10+1)})
	e, err := BuildReviewEvidence(t.Context(), root, a, b, 2)
	if err != nil {
		t.Fatal(err)
	}
	if e.Complete || e.ChangesTotal != 5 || e.OmittedChanges != 0 || e.PlanVersion != 2 || strings.Join(e.OmissionReasons, ",") != "binary,file_size" {
		t.Fatalf("evidence: %+v", e)
	}
	byPath := map[string]ReviewFileEvidence{}
	for _, item := range e.Changes {
		byPath[item.Path] = item
	}
	if byPath["add.txt"].Status != "added" || !strings.Contains(byPath["add.txt"].Diff, "+new\n") ||
		byPath["delete.txt"].Status != "deleted" || !strings.Contains(byPath["delete.txt"].Diff, "-gone\n") ||
		!byPath["edit.txt"].Complete || !strings.Contains(byPath["edit.txt"].Diff, "-before\n+after\n") ||
		byPath["image.bin"].Omission != "binary" || byPath["large.txt"].Omission != "file_size" {
		t.Fatalf("wrong change evidence: %+v", e.Changes)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "never in prompt") || strings.Contains(string(raw), ".git/config") {
		t.Fatal("Git metadata leaked")
	}
	if e.Validate(b.ID, 2) != nil || e.Validate(a.ID, 2) == nil || e.Validate(b.ID, 3) == nil {
		t.Fatal("snapshot/version not pinned")
	}
	e.Complete = true
	if e.Validate(b.ID, 2) == nil {
		t.Fatal("incomplete evidence marked complete")
	}
}

func TestReviewEvidenceInventoryBudgetAndTamperDetection(t *testing.T) {
	storage := t.TempDir()
	root, err := safefs.Open(storage)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	a := evidenceSnapshot(t, root, strings.Repeat("a", 32), nil)
	files := map[string]string{}
	for i := 0; i < 300; i++ {
		files[fmt.Sprintf("file-%03d", i)] = "new\n"
	}
	b := evidenceSnapshot(t, root, strings.Repeat("b", 32), files)
	e, err := BuildReviewEvidence(t.Context(), root, a, b, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	if e.Complete || len(e.Changes) != 256 || e.ChangesTotal != 300 || e.OmittedChanges != 44 || len(raw) > MaxReviewEvidenceBytes || strings.Join(e.OmissionReasons, ",") != "inventory_budget" {
		t.Fatalf("unbounded/misleading inventory: %+v", e)
	}
	// The bounded reader independently catches a post-verification mutation.
	file := filepath.Join(storage, b.ID, "tree", "file-000")
	if err = os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("evil"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = BuildReviewEvidence(t.Context(), root, a, b, 1); err == nil {
		t.Fatal("tampered snapshot read accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = BuildReviewEvidence(ctx, root, a, b, 1); err != context.Canceled {
		t.Fatal("cancel lost", err)
	}
}

func TestCompactReviewDiffIndependentHunksAndBudgets(t *testing.T) {
	var old, current strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&old, "line %d\n", i)
		if i == 2 || i == 90 {
			fmt.Fprintf(&current, "changed %d\n", i)
		} else {
			fmt.Fprintf(&current, "line %d\n", i)
		}
	}
	full, err := compactReviewDiff(t.Context(), "quoted\nname.txt", old.String(), current.String(), 32<<10)
	if err != nil || full.Hunks != 2 || full.IncludedHunks != 2 || full.Omission != "" || strings.Contains(full.Text, "line 50") || !strings.Contains(full.Text, `quoted\nname.txt`) {
		t.Fatal(full, err)
	}
	firstBoundary := strings.Index(full.Text[strings.Index(full.Text, "@@")+2:], "@@ -") + strings.Index(full.Text, "@@") + 2
	limited, err := compactReviewDiff(t.Context(), "quoted\nname.txt", old.String(), current.String(), firstBoundary)
	if err != nil || limited.Hunks != 2 || limited.IncludedHunks != 1 || limited.Omission != "diff_budget" || !strings.HasPrefix(full.Text, limited.Text) {
		t.Fatal("hunk truncation concealed", limited, err)
	}
	for _, pair := range [][2]string{{"", "new"}, {"gone", ""}, {"same\n", "different"}, {"", strings.Repeat("line\n", 300)}} {
		diff, err := compactReviewDiff(t.Context(), "file", pair[0], pair[1], 32<<10)
		if err != nil || diff.Hunks != 1 || diff.IncludedHunks != 1 || diff.Omission != "" {
			t.Fatal(pair, diff, err)
		}
		if pair[0] == "gone" && !strings.Contains(diff.Text, "-1,1 +0,0") {
			t.Fatal(diff.Text)
		}
	}
	diff, err := compactReviewDiff(t.Context(), "file", strings.Repeat("old\n", 300), strings.Repeat("new\n", 300), 32<<10)
	if err != nil || diff.Omission != "diff_complexity" || diff.Text != "" {
		t.Fatal("complexity omission hidden", diff, err)
	}
}

func TestReviewEditScriptPreservesBothDocuments(t *testing.T) {
	// Exhaustive short repeated-line inputs exercise ambiguous alignments and
	// insert/delete boundaries. Reading the script in either direction must
	// reconstruct the exact original document, including terminal newlines.
	values := [][]string{nil}
	for length := 1; length <= 4; length++ {
		for mask := 0; mask < 1<<length; mask++ {
			lines := make([]string, length)
			for i := range lines {
				if mask&(1<<i) == 0 {
					lines[i] = "a\n"
				} else {
					lines[i] = "b\n"
				}
			}
			values = append(values, lines)
		}
	}
	for _, a := range values {
		for _, b := range values {
			edits, ok, err := reviewEditScript(t.Context(), a, b)
			if err != nil || !ok {
				t.Fatal(err)
			}
			var before, after strings.Builder
			for _, line := range edits {
				if line.op != '+' {
					before.WriteString(line.text)
				}
				if line.op != '-' {
					after.WriteString(line.text)
				}
			}
			if before.String() != strings.Join(a, "") || after.String() != strings.Join(b, "") {
				t.Fatalf("invalid edit script: %q -> %q: %+v", a, b, edits)
			}
			diff, err := compactReviewDiff(t.Context(), "file", before.String(), after.String(), 1<<20)
			if err != nil || diff.Omission != "" {
				t.Fatal(diff, err)
			}
			if got := applyReviewDiff(t, before.String(), diff.Text); got != after.String() {
				t.Fatalf("diff does not reconstruct target: %q -> %q: %s", before.String(), after.String(), diff.Text)
			}
		}
	}
}

// Independent unified-diff consumer verifies coordinates, line counts and
// context instead of trusting the edit algorithm's internal representation.
func applyReviewDiff(t *testing.T, before, patch string) string {
	t.Helper()
	if patch == "" {
		return before
	}
	original := strings.SplitAfter(before, "\n")
	if original[len(original)-1] == "" {
		original = original[:len(original)-1]
	}
	lines := strings.SplitAfter(patch, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	cursor := 0
	var out strings.Builder
	for at := 2; at < len(lines); {
		var oldStart, oldCount, newStart, newCount int
		if n, err := fmt.Sscanf(strings.TrimSuffix(lines[at], "\n"), "@@ -%d,%d +%d,%d @@", &oldStart, &oldCount, &newStart, &newCount); err != nil || n != 4 {
			t.Fatalf("invalid hunk: %s", lines[at])
		}
		start := oldStart
		if oldCount > 0 {
			start--
		}
		if start < cursor || start > len(original) {
			t.Fatal("invalid original position", start, cursor)
		}
		out.WriteString(strings.Join(original[cursor:start], ""))
		cursor = start
		at++
		oldRead, newWritten := 0, 0
		for at < len(lines) && !strings.HasPrefix(lines[at], "@@ ") {
			line := lines[at]
			at++
			if len(line) < 1 {
				t.Fatal("empty diff line")
			}
			op, text := line[0], line[1:]
			if at < len(lines) && strings.HasPrefix(lines[at], "\\ No newline") {
				text = strings.TrimSuffix(text, "\n")
				at++
			}
			if op != '+' {
				if cursor >= len(original) || original[cursor] != text {
					t.Fatalf("context mismatch at %d: %q", cursor, text)
				}
				cursor++
				oldRead++
			}
			if op != '-' {
				out.WriteString(text)
				newWritten++
			}
		}
		if oldRead != oldCount || newWritten != newCount {
			t.Fatal("hunk line counts incorrect")
		}
	}
	out.WriteString(strings.Join(original[cursor:], ""))
	return out.String()
}
