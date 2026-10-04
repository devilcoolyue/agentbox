package syncfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncproto"
)

func fileEntry(value string) syncproto.Entry {
	return syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte(value)), Size: int64(len(value))}
}

func TestConditionalReplaceAndDeletePreserveRecovery(t *testing.T) {
	r, dir := fixtureRoot(t)
	w := Writer{Root: r}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	old, new := fileEntry("old"), fileEntry("new\r\n\x00")
	if _, err := w.Replace(t.Context(), "sub/file", nil, old, bytes.NewBufferString("old")); err != nil {
		t.Fatal(err)
	}
	recovery, err := w.Replace(t.Context(), "sub/file", &old, new, bytes.NewBufferString("new\r\n\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(recovery.Path))); err != nil || string(data) != "old" {
		t.Fatalf("backup: %q %v", data, err)
	}
	if _, err = w.Replace(t.Context(), "sub/file", &old, new, bytes.NewBufferString("new\r\n\x00")); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("stale precondition accepted", err)
	}
	if _, err = w.Delete(t.Context(), "sub/file", old); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("stale deletion accepted", err)
	}
	recovery, err = w.Delete(t.Context(), "sub/file", new)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "sub/file")); !os.IsNotExist(err) {
		t.Fatal("delete failed", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(recovery.Path))); err != nil || string(data) != "new\r\n\x00" {
		t.Fatalf("deleted backup: %q %v", data, err)
	}
	rules, _ := syncproto.ParseRules("")
	manifest, err := r.Scan(t.Context(), rules, Limits{})
	if err != nil || len(manifest.Entries) != 1 {
		t.Fatalf("recovery leaked into sync: %+v %v", manifest, err)
	}
}

type callbackReader struct {
	fn     func()
	reader io.Reader
}

func (r *callbackReader) Read(p []byte) (int, error) {
	if r.fn != nil {
		r.fn()
		r.fn = nil
	}
	return r.reader.Read(p)
}

func TestConcurrentEditAndInterruptedTransferNeverOverwrite(t *testing.T) {
	r, dir := fixtureRoot(t)
	w := Writer{Root: r}
	old, new := fileEntry("old"), fileEntry("new")
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	source := &callbackReader{fn: func() {
		if err := os.WriteFile(file, []byte("editor"), 0644); err != nil {
			t.Fatal(err)
		}
	}, reader: bytes.NewBufferString("new")}
	if _, err := w.Replace(t.Context(), "file", &old, new, source); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("concurrent editor overwritten", err)
	}
	if data, _ := os.ReadFile(file); string(data) != "editor" {
		t.Fatal(string(data))
	}
	expected := fileEntry("editor")
	if _, err := w.Replace(t.Context(), "file", &expected, new, bytes.NewBufferString("wrong hash")); err == nil {
		t.Fatal("bad download accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := w.Replace(ctx, "file", &expected, new, bytes.NewBufferString("new")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); string(data) != "editor" {
		t.Fatal("failed transfer changed destination")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("staging leak: %+v", entries)
	}
}

func TestAbsentPreconditionAndReservedPaths(t *testing.T) {
	r, dir := fixtureRoot(t)
	w := Writer{Root: r}
	new := fileEntry("new")
	source := &callbackReader{fn: func() {
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte("created"), 0644); err != nil {
			t.Fatal(err)
		}
	}, reader: bytes.NewBufferString("new")}
	if _, err := w.Replace(t.Context(), "file", nil, new, source); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("concurrent creation overwritten", err)
	}
	for _, name := range []string{".git/config", ".agentbox-sync/recovery/x", "../outside"} {
		if _, err := w.Replace(t.Context(), name, nil, new, bytes.NewBufferString("new")); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("reserved path %s: %v", name, err)
		}
	}
}
