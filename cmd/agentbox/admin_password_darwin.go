package main

import "golang.org/x/sys/unix"

func flushPasswordInput(fd uintptr) error {
	return unix.IoctlSetPointerInt(int(fd), unix.TIOCFLUSH, unix.TCIFLUSH)
}
