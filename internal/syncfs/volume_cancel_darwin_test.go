package syncfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Exercise the production command runner with a real blocked child. The
// executable substitution exists only in this test; runtime diskutil remains a
// fixed system path and cannot be overridden by environment or IPC.
func TestDarwinVolumeQueryCancellationReapsChild(t *testing.T) {
	if os.Getenv("AGENTBOX_VOLUME_QUERY_CHILD") == "1" {
		if err := os.WriteFile(os.Getenv("AGENTBOX_VOLUME_QUERY_READY"), []byte("ready"), 0600); err != nil {
			os.Exit(19)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDarwinVolumeQueryCancellationReapsChild$")
	command.Env = append(os.Environ(), "AGENTBOX_VOLUME_QUERY_CHILD=1", "AGENTBOX_VOLUME_QUERY_READY="+ready)
	finished := make(chan error, 1)
	go func() { _, err := runDiskInfoCommand(ctx, command); finished <- err }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatal("fixture query exited before cancellation", err)
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("fixture query failed to start")
		}
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("query cancellation was lost", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request cancellation did not reap its blocked helper before sidecar's 3-second grace period")
	}
	if command.ProcessState == nil {
		t.Fatal("query child was not waited on")
	}
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("query child was not killed and reaped by context cancellation")
	}
}

func TestDarwinUnknownFilesystemRejectedBeforeMetadataLookup(t *testing.T) {
	mount, _ := darwinDiskFixture()
	clear(mount.Fstypename[:])
	copy(mount.Fstypename[:], "macfuse")
	if err := checkDarwinVolume(t.Context(), mount, func(context.Context, string) (diskInfo, error) {
		t.Fatal("unknown filesystem attempted to borrow a physical device's metadata")
		return nil, nil
	}); !errors.Is(err, ErrUnsupportedVolume) {
		t.Fatal(err)
	}
}
