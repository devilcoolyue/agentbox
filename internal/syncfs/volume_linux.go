package syncfs

import (
	"context"
	"golang.org/x/sys/unix"
)

func checkVolume(ctx context.Context, name string) (volumeGuard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(name, &stat); err != nil {
		return nil, err
	}
	if stat.Type == unix.NFS_SUPER_MAGIC || stat.Type == 0xff534d42 || stat.Type == 0xfe534d42 {
		return nil, ErrUnsupportedVolume
	}
	return nil, nil
}
