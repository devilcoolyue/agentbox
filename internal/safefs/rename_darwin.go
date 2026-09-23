//go:build darwin

package safefs

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameAt(src *os.File, name string, dst *os.File, target string, replace bool) error {
	flags := uint32(0)
	if !replace {
		flags = unix.RENAME_EXCL
	}
	err := unix.RenameatxNp(int(src.Fd()), name, int(dst.Fd()), target, flags)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: name, New: target, Err: err}
	}
	return nil
}
