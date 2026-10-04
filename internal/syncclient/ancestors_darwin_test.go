package syncclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncfs"
)

func TestAncestorOverlapAcrossDarwinFirmlink(t *testing.T) {
	state, saved, _ := stateFixture(t)
	// macOS Data-volume firmlinks are native aliases, not symlinks. Raw
	// spelling/prefix checks cannot detect overlap between these two paths.
	alias := filepath.Join("/System/Volumes/Data", saved.Directory)
	originalInfo, err := os.Stat(saved.Directory)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(originalInfo, aliasInfo) {
		t.Skip("this machine does not expose the APFS system/Data firmlink alias")
	}
	child := filepath.Join(saved.Directory, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	aliasChild := filepath.Join(alias, "child")
	if pathsOverlap(aliasChild, saved.Directory) {
		t.Fatal("fixture did not bypass spelling-only overlap checks")
	}
	root, err := syncfs.OpenContext(t.Context(), aliasChild)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	identity, err := root.Identity()
	if err != nil {
		t.Fatal(err)
	}
	binding := saved.Binding
	binding.LocalID = identity
	binding.Workspace = "separate-remote-workspace"
	if _, err := state.RegisterContext(t.Context(), binding, aliasChild); !errors.Is(err, ErrBinding) {
		t.Fatal("firmlink child bypassed native ancestor overlap checks", err)
	}
	for _, path := range []string{alias, aliasChild, filepath.Dir(alias)} {
		if target, err := state.exportDestination(t.Context(), path); err == nil {
			target.Close()
			t.Fatal("recovery export accepted a firmlink alias overlapping the mapped tree", path)
		}
	}
	// A sibling on the same Data volume is still a valid export destination.
	sibling, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := state.exportDestination(t.Context(), sibling)
	if err != nil {
		t.Fatal("non-overlapping export was rejected", err)
	}
	target.Close()
}
