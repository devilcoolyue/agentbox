//go:build !linux

package server

import (
	"os"
	"path/filepath"
)

// The production target is Linux. This fallback keeps development and tests
// portable; the caller already performs a conflict check before this point.
func renameNoReplace(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return &os.PathError{Op: "rename", Path: destination, Err: os.ErrExist}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(filepath.Clean(source), filepath.Clean(destination))
}
