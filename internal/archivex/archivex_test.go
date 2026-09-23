package archivex

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func archiveFixture(t *testing.T, format string, entries map[string]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "archive-*"+format)
	if err != nil {
		t.Fatal(err)
	}
	if format == ".zip" {
		w := zip.NewWriter(f)
		for name, body := range entries {
			entry, err := w.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(entry, body); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		w := tar.NewWriter(f)
		for name, body := range entries {
			if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(w, body); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestExtractRejectsExistingSymlinkAndTraversal(t *testing.T) {
	for _, format := range []string{".zip", ".tar"} {
		t.Run(format, func(t *testing.T) {
			outside := t.TempDir()
			secret := filepath.Join(outside, "value")
			if err := os.WriteFile(secret, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"link/value", "../value", "/absolute/value", "value"} {
				dst := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(dst, "link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(secret, filepath.Join(dst, "value")); err != nil {
					t.Fatal(err)
				}
				src := archiveFixture(t, format, map[string]string{name: "overwrite"})
				if _, err := Extract(src, "upload"+format, dst, 1024); err == nil {
					t.Fatalf("accepted %q", name)
				}
				if got, _ := os.ReadFile(secret); string(got) != "keep" {
					t.Fatalf("escaped: %q", got)
				}
			}
		})
	}
}

func TestExtractLimitsAndRoundTrip(t *testing.T) {
	for _, format := range []string{".zip", ".tar"} {
		t.Run(format, func(t *testing.T) {
			src := archiveFixture(t, format, map[string]string{"nested/file": "hello", "empty": ""})
			if _, err := Extract(src, "upload"+format, t.TempDir(), 4); err == nil {
				t.Fatal("size limit bypassed")
			}
			dst := t.TempDir()
			if n, err := Extract(src, "upload"+format, dst, 5); err != nil || n != 2 {
				t.Fatalf("extract=%d %v", n, err)
			}
			outside := filepath.Join(t.TempDir(), "secret")
			_ = os.WriteFile(outside, []byte("not in archive"), 0o644)
			_ = os.Symlink(outside, filepath.Join(dst, "link"))
			var buf bytes.Buffer
			if err := ZipDir(&buf, dst); err != nil {
				t.Fatal(err)
			}
			r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
			if err != nil {
				t.Fatal(err)
			}
			if len(r.File) != 2 {
				t.Fatalf("unexpected zip entries: %v", r.File)
			}
			for _, f := range r.File {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				if f.Name == "nested/file" && string(data) != "hello" {
					t.Fatalf("bad contents %q", data)
				}
			}
		})
	}
}
