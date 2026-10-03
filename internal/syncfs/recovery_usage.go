package syncfs

import (
	"context"
	"errors"
	"io"
	"os"

	"agentbox/internal/syncproto"
)

// RecoveryUsage describes only the current project-wide recovery directory.
// Legacy per-parent copies, exports and filesystem allocation are not counted.
type RecoveryUsage struct {
	Files     int   `json:"files"`
	Bytes     int64 `json:"bytes"`
	FileLimit int   `json:"file_limit"`
	ByteLimit int64 `json:"byte_limit"`
}

func (r *Root) RecoveryUsage(ctx context.Context) (RecoveryUsage, error) {
	usage := RecoveryUsage{FileLimit: MaxRecoveryFiles, ByteLimit: MaxRecoveryBytes}
	if err := ctx.Err(); err != nil {
		return RecoveryUsage{}, err
	}
	if err := r.CheckIdentity(); err != nil {
		return RecoveryUsage{}, err
	}
	recovery, err := r.sub(".agentbox-sync/recovery")
	if os.IsNotExist(err) {
		return usage, r.CheckIdentity()
	}
	if err != nil {
		return RecoveryUsage{}, err
	}
	defer recovery.Close()
	directory, err := recovery.root.Open(".")
	if err != nil {
		return RecoveryUsage{}, err
	}
	entries, readErr := directory.ReadDir(MaxRecoveryFiles + 1)
	directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return RecoveryUsage{}, readErr
	}
	if len(entries) > MaxRecoveryFiles {
		return RecoveryUsage{}, syncproto.ErrLimit
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return RecoveryUsage{}, err
		}
		file, err := recovery.OpenFile(entry.Name())
		if err != nil {
			return RecoveryUsage{}, err
		}
		info, err := file.Stat()
		file.Close()
		if err != nil {
			return RecoveryUsage{}, err
		}
		usage.Files++
		usage.Bytes += info.Size()
	}
	return usage, r.CheckIdentity()
}
