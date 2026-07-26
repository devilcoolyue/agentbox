//go:build linux

package server

import "golang.org/x/sys/unix"

// renameNoReplace keeps the conflict check atomic against files created by a
// running session container while the move request is in flight.
func renameNoReplace(source, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}
