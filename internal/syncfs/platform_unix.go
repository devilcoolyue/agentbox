//go:build darwin || linux

package syncfs

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func directoryIdentity(root *os.Root) (string, error) {
	f, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer f.Close()
	var stat unix.Stat_t
	if err = unix.Fstat(int(f.Fd()), &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}

func openRegular(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

func syncDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func checkSingleLink(f *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return err
	}
	if stat.Nlink != 1 {
		return ErrUnsafe
	}
	return nil
}

// Explicit flags are essential: os.Root.Remove can remove either type.
func removeTyped(root *os.Root, name string, expected os.FileInfo, directory bool) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, info) || info.IsDir() != directory {
		return ErrUnsafe
	}
	parent, err := root.Open(".")
	if err != nil {
		return err
	}
	defer parent.Close()
	flags := 0
	if directory {
		flags = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(int(parent.Fd()), name, flags)
}
