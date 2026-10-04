package syncfs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"agentbox/internal/syncproto"
)

// AncestorIdentitiesContext returns the selected directory first, followed by
// each parent up to its platform path root, using the same IDs as Identity.
// It only reads identities from pinned handles; it neither returns ancestor
// access nor approves those ancestors as sync roots. In particular, macOS's
// system/data firmlink ancestors can cross mounts while the selected root must
// still match its original approved volume and directory identity.
func (r *Root) AncestorIdentitiesContext(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.absolute == "" || r.identity == nil {
		return nil, ErrUnsafe
	}
	original, err := r.root.Stat(".")
	if err != nil || !os.SameFile(original, r.identity) {
		return nil, ErrRootChanged
	}
	if err := r.CheckIdentity(); err != nil {
		return nil, err
	}
	anchor := filepath.VolumeName(r.absolute) + string(filepath.Separator)
	start, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	// Keep the entire chain pinned until its parent/name relationships have
	// been rechecked. Do not walk '..' from a directory that may have moved.
	pins := []*Root{{root: start}}
	defer func() {
		for i := len(pins) - 1; i >= 0; i-- {
			pins[i].Close()
		}
	}()
	var components []string
	for _, part := range strings.Split(strings.TrimPrefix(r.absolute, anchor), string(filepath.Separator)) {
		if part != "" {
			components = append(components, part)
		}
	}
	if len(components)+1 > 256 {
		return nil, syncproto.ErrLimit
	}
	for _, part := range components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// These temporary roots deliberately have no sync-volume guard: only
		// the original selected root grants access to content or mutations.
		// sub still checks every component for links/reparse points and races.
		next, err := pins[len(pins)-1].sub(part)
		if err != nil {
			return nil, err
		}
		pins = append(pins, next)
	}
	identities := make([]string, len(pins))
	infos := make([]os.FileInfo, len(pins))
	for i, pin := range pins {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := pin.root.Stat(".")
		if err != nil {
			return nil, err
		}
		infos[i] = info
		id, err := directoryIdentity(pin.root)
		if err != nil {
			return nil, err
		}
		identities[i] = syncproto.HashBytes([]byte(runtime.GOOS + ":" + id))
	}
	last := len(pins) - 1
	if !os.SameFile(infos[last], r.identity) {
		return nil, ErrRootChanged
	}
	if r.volume != nil {
		if err := r.volume(pins[last].root); err != nil {
			return nil, ErrRootChanged
		}
	}
	for i := 1; i < len(pins); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, err := pins[i-1].root.Lstat(components[i-1])
		if err != nil || !current.IsDir() || current.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !os.SameFile(current, infos[i]) {
			return nil, ErrRootChanged
		}
	}
	if err := r.CheckIdentity(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(identities)
	return identities, nil
}
