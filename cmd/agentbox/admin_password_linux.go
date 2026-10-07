package main

import "golang.org/x/sys/unix"

func flushPasswordInput(fd uintptr) error {
	return unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH)
}
