package syncfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"agentbox/internal/syncproto"
)

// Opt in because this creates and mounts an isolated 32 MiB test image. It never
// formats a physical device or modifies existing disks, and detaches on failure.
func TestDarwinDiskImageAndNestedMountRejected(t *testing.T) {
	if os.Getenv("AGENTBOX_TEST_DARWIN_DISK_IMAGE") != "1" {
		t.Skip("set AGENTBOX_TEST_DARWIN_DISK_IMAGE=1 for isolated disk-image acceptance")
	}
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(temporary, "project")
	mountpoint := filepath.Join(directory, "nested")
	if err := os.MkdirAll(mountpoint, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	selected, err := Open(mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Close()
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "/usr/bin/hdiutil", args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("hdiutil %s failed: %v: %s", args[0], err, output)
		}
	}
	image := filepath.Join(temporary, "volume-policy.dmg")
	run("create", "-size", "32m", "-fs", "HFS+", "-volname", "AgentboxSyncTest", image)
	run("attach", "-nobrowse", "-mountpoint", mountpoint, image)
	t.Cleanup(func() { run("detach", mountpoint) })

	if accepted, err := Open(mountpoint); err == nil {
		accepted.Close()
		t.Fatal("disk image accepted as sync root")
	}
	if ids, err := selected.AncestorIdentitiesContext(t.Context()); err == nil || ids != nil {
		t.Fatal("mount replacement retained ancestor access", ids, err)
	}
	if err := root.CheckIdentity(); err != nil {
		t.Fatal("internal project root changed", err)
	}
	rules, _ := syncproto.ParseRules("")
	manifest, err := root.Scan(t.Context(), rules, Limits{})
	if err == nil || manifest.Entries != nil {
		t.Fatalf("nested mount accepted or partial manifest returned: %v", err)
	}
	if accepted, err := root.sub("nested"); err == nil {
		accepted.Close()
		t.Fatal("nested disk image bypassed root policy")
	}
}
