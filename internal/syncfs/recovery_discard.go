package syncfs

import (
	"context"
	"os"
	"strings"

	"agentbox/internal/syncproto"
)

// DiscardRecovery removes only an exact, previously journaled recovery copy.
// The caller must persist disposal intent first. Missing files are idempotent
// after a crash between unlink and the final metadata transaction. No project
// file, unknown orphan, directory, symlink, or hardlink is eligible.
func (r *Root) DiscardRecovery(ctx context.Context, copy Recovery) error {
	if !syncproto.ValidPath(copy.Path) || !strings.HasPrefix(copy.Path, ".agentbox-sync/recovery/") || strings.Count(copy.Path, "/") != 2 || !strings.HasSuffix(copy.Path, ".bak") || !syncproto.ValidHash(copy.Hash) || copy.Size < 0 || copy.Size > syncproto.DefaultMaxFileBytes {
		return syncproto.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.CheckIdentity(); err != nil {
		return err
	}
	parent, leaf, err := r.parent(copy.Path)
	if os.IsNotExist(err) {
		return r.CheckIdentity()
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	file, err := parent.OpenFile(leaf)
	if os.IsNotExist(err) {
		return r.CheckIdentity()
	}
	if err != nil {
		return err
	}
	identity, err := file.Stat()
	file.Close() // Windows permits deletion only after handles are closed.
	if err != nil {
		return err
	}
	expected := syncproto.Entry{Kind: "file", Hash: copy.Hash, Size: copy.Size}
	if err = checkExpected(ctx, parent, leaf, &expected); err != nil {
		return err
	}
	if err = r.CheckIdentity(); err != nil {
		return err
	}
	fresh, err := r.sub(".agentbox-sync/recovery")
	if err != nil {
		return err
	}
	defer fresh.Close()
	pinned, err := parent.root.Stat(".")
	if err != nil {
		return err
	}
	current, err := fresh.root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(pinned, current) {
		return ErrRootChanged
	}
	actual, err := parent.root.Lstat(leaf)
	if err != nil {
		return err
	}
	if !os.SameFile(identity, actual) {
		return syncproto.ErrChanged
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = parent.root.Remove(leaf); err != nil {
		return err
	}
	if err = syncDirectory(parent.root); err != nil {
		return err
	}
	return r.CheckIdentity()
}
