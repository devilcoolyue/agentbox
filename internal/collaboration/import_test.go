package collaboration

import (
	"archive/tar"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"agentbox/internal/safefs"
)

func fixtureBlob(text string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(text))
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

func TestGitArchiveMatchesCommittedTreeAndRejectsAlterations(t *testing.T) {
	for _, mode := range []string{"valid", "omitted", "substituted", "unexpected", "link-escape", "lfs", "hardlink", "unexpected-directory"} {
		t.Run(mode, func(t *testing.T) {
			content := "fixture"
			if mode == "lfs" {
				content = "version https://git-lfs.github.com/spec/v1\noid sha256:0000\nsize 1\n"
			}
			raw := fmt.Sprintf("100644 blob %s %d\tfile\x00", fixtureBlob(content), len(content))
			limits := SnapshotLimits{MaxBytes: 1024, MaxEntries: 10}
			tree, err := ParseGitTree([]byte(raw), limits)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if mode != "omitted" {
				header := &tar.Header{Name: "file", Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0644}
				data := content
				switch mode {
				case "substituted":
					data = "changed"
				case "unexpected":
					header.Name = "other"
				case "hardlink":
					header.Typeflag = tar.TypeLink
					header.Linkname = "other"
					header.Size = 0
					data = ""
				case "link-escape":
					header.Name = "../outside"
				}
				if err = tw.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
				if _, err = tw.Write([]byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unexpected-directory" {
				if err = tw.WriteHeader(&tar.Header{Name: "uncommitted/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
					t.Fatal(err)
				}
			}
			if err = tw.Close(); err != nil {
				t.Fatal(err)
			}
			root, err := safefs.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = ImportGitArchive(t.Context(), bytes.NewReader(buf.Bytes()), root, tree, limits)
			if (err == nil) != (mode == "valid") {
				t.Fatal(mode, err)
			}
			if mode == "valid" {
				got, e := root.ReadAll("file", 100)
				if e != nil || string(got) != content {
					t.Fatal(string(got), e)
				}
			}
		})
	}
}

func TestGitTreeRefusesUnsafeAndIncompleteInputs(t *testing.T) {
	limits := SnapshotLimits{MaxBytes: 100, MaxEntries: 10}
	valid := "100644 blob " + strings.Repeat("a", 40) + " 5\tfile\x00"
	for _, raw := range []string{strings.TrimSuffix(valid, "\x00"), strings.Replace(valid, "file", "../outside", 1), strings.Replace(valid, "file", ".git/config", 1), strings.Replace(valid, "blob", "commit", 1), strings.Replace(valid, " 5\t", " 1000\t", 1), valid + valid} {
		if _, err := ParseGitTree([]byte(raw), limits); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
