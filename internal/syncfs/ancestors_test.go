package syncfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"agentbox/internal/syncproto"
)

func TestAncestorIdentitiesMatchNativeChainAndCancellation(t *testing.T) {
	root, directory := fixtureRoot(t)
	if err := os.WriteFile(filepath.Join(directory, "keep"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	identities, err := root.AncestorIdentitiesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var expected []string
	for path := directory; ; path = filepath.Dir(path) {
		// Independent native-handle oracle: keep the existing platform ID
		// encoding and self-to-volume-root ordering exactly stable.
		native, err := os.OpenRoot(path)
		if err != nil {
			t.Fatal(err)
		}
		id, err := directoryIdentity(native)
		native.Close()
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, syncproto.HashBytes([]byte(runtime.GOOS+":"+id)))
		if filepath.Dir(path) == path {
			break
		}
	}
	self, err := root.Identity()
	if err != nil || len(identities) == 0 || identities[0] != self || !reflect.DeepEqual(identities, expected) {
		t.Fatal("native ancestor identities or ordering changed", identities, expected, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatal("ancestor inspection wrote probes or modified files", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ids, err := root.AncestorIdentitiesContext(ctx); ids != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation returned partial identities", ids, err)
	}
	root.Close()
	if ids, err := root.AncestorIdentitiesContext(t.Context()); ids != nil || !errors.Is(err, ErrRootChanged) {
		t.Fatal("closed original pin granted ancestor access", ids, err)
	}
}

func TestAncestorIdentitiesRefreshParentAfterMove(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("moving pinned directories requires separate Windows sharing-mode acceptance")
	}
	_, directory := fixtureRoot(t)
	parent := filepath.Join(directory, "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenContext(t.Context(), child)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	old, err := root.AncestorIdentitiesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(directory, "moved-parent")
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(moved, "child"), child); err != nil {
		t.Fatal(err)
	}
	current, err := root.AncestorIdentitiesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != len(old) || current[0] != old[0] || current[1] == old[1] || !reflect.DeepEqual(current[2:], old[2:]) {
		t.Fatal("reusing a selected directory cached its replaced parent's identity", old, current)
	}
}

func TestAncestorIdentitiesRejectMissingReplacedAndLinkedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("moving pinned directories requires separate Windows sharing-mode acceptance")
	}
	for _, kind := range []string{"missing", "replacement", "link"} {
		t.Run(kind, func(t *testing.T) {
			root, directory := fixtureRoot(t)
			moved := directory + "-moved"
			if err := os.Rename(directory, moved); err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(moved)
			switch kind {
			case "replacement":
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			case "link":
				if err := os.Symlink(moved, directory); err != nil {
					t.Fatal(err)
				}
			}
			if ids, err := root.AncestorIdentitiesContext(t.Context()); ids != nil || !errors.Is(err, ErrRootChanged) {
				t.Fatal("changed selected path produced usable ancestors", ids, err)
			}
		})
	}
}

func TestAncestorIdentitiesBoundDepthWithoutPartialResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows path-length acceptance is independent of the ancestor count limit")
	}
	_, directory := fixtureRoot(t)
	deep := filepath.Join(directory, strings.Repeat("d/", 260))
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenContext(t.Context(), deep)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if ids, err := root.AncestorIdentitiesContext(t.Context()); ids != nil || !errors.Is(err, syncproto.ErrLimit) {
		t.Fatal("ancestor bound returned partial IDs or did not stop", len(ids), err)
	}
}
