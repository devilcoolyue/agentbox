package server

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// fileBirth is when the open file was created, where the filesystem records
// it (ext4, xfs, btrfs); ok is false otherwise.
func fileBirth(f *os.File) (birth time.Time, ok bool) {
	conn, err := f.SyscallConn()
	if err != nil {
		return time.Time{}, false
	}
	var stx unix.Statx_t
	var serr error
	if err := conn.Control(func(fd uintptr) {
		serr = unix.Statx(int(fd), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stx)
	}); err != nil || serr != nil || stx.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), true
}
