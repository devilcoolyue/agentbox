package syncclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncfs"
)

func TestCanceledNativeDirectoryOpenDoesNotCreateState(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	missing := filepath.Join(t.TempDir(), "uncreated-state")
	if root, err := syncfs.OpenContext(ctx, missing); root != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("filesystem open ignored the request cancellation", err)
	}
	if state, err := OpenStateContext(ctx, missing); state != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("state open ignored the request cancellation", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("canceled state open touched the filesystem", err)
	}
	if _, err := InspectLocal(ctx, missing); !errors.Is(err, context.Canceled) {
		t.Fatal("inspection did not forward request cancellation", err)
	}
}

func TestStateStoreDoesNotRetainOpeningRequestContext(t *testing.T) {
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	state, err := OpenStateContext(ctx, filepath.Join(private, "state"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer state.Close()
	cancel()
	if _, err := state.root.AncestorIdentitiesContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("ancestor inspection did not forward request cancellation", err)
	}
	if _, err := state.Device(); err != nil {
		t.Fatal("completed opening request canceled later independent work", err)
	}
	missing := filepath.Join(private, "uncreated-export")
	if _, err := state.exportDestination(ctx, missing); !errors.Is(err, context.Canceled) {
		t.Fatal("export destination did not forward its own request cancellation", err)
	}
}
