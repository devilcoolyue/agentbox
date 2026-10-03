package syncclient

import (
	"context"
	"io"
	"os"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

// scanLocal snapshots the ignore policy around a complete scan, including
// existence changes for an empty ignore file. It never returns a partial tree.
func scanLocal(ctx context.Context, root *syncfs.Root, caps syncfs.Capabilities) (syncproto.Manifest, error) {
	raw, exists, err := readLocalRules(root)
	if err != nil {
		return syncproto.Manifest{}, err
	}
	rules, err := syncproto.ParseRules(string(raw))
	if err != nil {
		return syncproto.Manifest{}, err
	}
	m, err := root.ScanWithProgress(ctx, rules, syncfs.Limits{}, func(path string, files int, bytes int64) { progressScan(ctx, path, files, bytes) })
	if err != nil {
		return syncproto.Manifest{}, err
	}
	after, nowExists, err := readLocalRules(root)
	if err != nil {
		return syncproto.Manifest{}, err
	}
	if exists != nowExists || string(raw) != string(after) {
		return syncproto.Manifest{}, syncproto.ErrChanged
	}
	if err = syncfs.RequireNames(m, caps.NamePolicy); err != nil {
		return syncproto.Manifest{}, err
	}
	return m, nil
}

func readLocalRules(root *syncfs.Root) ([]byte, bool, error) {
	f, err := root.OpenFile(".agentboxignore")
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > 64<<10 {
		return nil, false, syncproto.ErrLimit
	}
	return b, true, nil
}
