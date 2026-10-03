package syncfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncproto"
)

func TestRecoveryUsageCountsCopiesAndRejectsUnavailable(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	usage, err := root.RecoveryUsage(t.Context())
	if err != nil || usage.Files != 0 || usage.FileLimit != MaxRecoveryFiles {
		t.Fatal(usage, err)
	}
	old := syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte("old")), Size: 3}
	new := syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte("new")), Size: 3}
	writer := Writer{Root: root}
	if _, err = writer.Replace(t.Context(), "file", nil, old, bytes.NewBufferString("old")); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Replace(t.Context(), "file", &old, new, bytes.NewBufferString("new")); err != nil {
		t.Fatal(err)
	}
	usage, err = root.RecoveryUsage(t.Context())
	if err != nil || usage.Files != 1 || usage.Bytes != 3 {
		t.Fatal(usage, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = root.RecoveryUsage(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if err = os.Mkdir(filepath.Join(directory, ".agentbox-sync", "recovery", "unexpected"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = root.RecoveryUsage(t.Context()); err == nil {
		t.Fatal("unsafe directory reported as known usage")
	}
}
