package syncfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path"

	"agentbox/internal/syncproto"
)

func (w *Writer) beforePublish(ctx context.Context, name string, parent *Root) error {
	if w.Guard != nil {
		if err := w.Guard(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.Root.CheckIdentity(); err != nil {
		return err
	}
	// A pinned child can have been moved out of the mapped tree by an editor.
	// Never deliberately write into that detached child after observing the move.
	current, err := w.Root.sub(path.Dir(name))
	if err != nil {
		return err
	}
	defer current.Close()
	want, err := parent.root.Stat(".")
	if err != nil {
		return err
	}
	actual, err := current.root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(want, actual) {
		return syncproto.ErrChanged
	}
	return nil
}

// Mkdir creates exactly one absent directory. Parents must already exist and
// pass the pinned no-link traversal; it never merges an existing directory.
func (w *Writer) Mkdir(ctx context.Context, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := mutationPath(name); err != nil {
		return err
	}
	if err := w.Root.CheckIdentity(); err != nil {
		return err
	}
	parent, leaf, err := w.Root.parent(name)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err = w.beforePublish(ctx, name, parent); err != nil {
		return err
	}
	if err = checkExpected(ctx, parent, leaf, nil); err != nil {
		return err
	}
	if err = parent.root.Mkdir(leaf, 0755); err != nil {
		return err
	}
	if err = syncDirectory(parent.root); err != nil {
		return err
	}
	created, err := parent.sub(leaf)
	if err != nil {
		return err
	}
	created.Close()
	return w.Root.CheckIdentity()
}

// Rmdir removes only an empty directory. Ignored files (including legacy
// per-parent recovery folders) still count as children and prevent deletion.
// The native primitive is type-restricted so a raced-in file cannot be unlinked.
func (w *Writer) Rmdir(ctx context.Context, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := mutationPath(name); err != nil {
		return err
	}
	if err := w.Root.CheckIdentity(); err != nil {
		return err
	}
	parent, leaf, err := w.Root.parent(name)
	if err != nil {
		return err
	}
	defer parent.Close()
	target, err := parent.sub(leaf)
	if err != nil {
		return err
	}
	info, err := target.root.Stat(".")
	if err != nil {
		target.Close()
		return err
	}
	f, err := target.root.Open(".")
	if err != nil {
		target.Close()
		return err
	}
	children, readErr := f.ReadDir(1)
	f.Close()
	target.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if len(children) != 0 {
		return errors.New("directory is not empty")
	}
	if err = w.beforePublish(ctx, name, parent); err != nil {
		return err
	}
	current, err := parent.root.Lstat(leaf)
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !os.SameFile(info, current) {
		return syncproto.ErrChanged
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = removeTyped(parent.root, leaf, info, true); err != nil {
		return err
	}
	if err = syncDirectory(parent.root); err != nil {
		return err
	}
	return checkExpected(ctx, parent, leaf, nil)
}
