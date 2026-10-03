package syncfs

import (
	"context"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

func directoryIdentity(root *os.Root) (string, error) {
	f, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func checkVolume(ctx context.Context, name string) (volumeGuard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(name)
	// Native Windows pickers and Rust canonicalize can return extended drive
	// paths (\\?\C:\...). They still identify a local drive, not a UNC share.
	if strings.HasPrefix(volume, `\\?\`) {
		drive := strings.TrimPrefix(volume, `\\?\`)
		if len(drive) != 2 || drive[1] != ':' || !(drive[0] >= 'a' && drive[0] <= 'z' || drive[0] >= 'A' && drive[0] <= 'Z') {
			return nil, ErrUnsupportedVolume
		}
		volume = drive
	}
	if strings.HasPrefix(volume, `\\`) {
		return nil, ErrUnsupportedVolume
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return nil, err
	}
	if windows.GetDriveType(root) != windows.DRIVE_FIXED {
		return nil, ErrUnsupportedVolume
	}
	return nil, nil
}

// Native relative open refuses the final reparse point and directories before
// any data read. Parent is an already pinned, link-checked os.Root.
func openRegular(root *os.Root, name string) (*os.File, error) {
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	un, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: un, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ|windows.SYNCHRONIZE, &oa, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), name)
	if err = checkSingleLink(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func checkSingleLink(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.NumberOfLinks != 1 {
		return ErrUnsafe
	}
	return nil
}

// Windows directory handles cannot use FlushFileBuffers like POSIX fsync.
// File contents are flushed before rename. Power-loss durability of the rename
// itself remains filesystem-dependent and requires rescan after recovery.
func syncDirectory(*os.Root) error { return nil }

// Delete by a relative, type-restricted handle. It neither traverses a reparse
// point nor falls back to pathname unlink when sharing/permissions refuse it.
func removeTyped(root *os.Root, name string, expected os.FileInfo, directory bool) error {
	parent, err := root.Open(".")
	if err != nil {
		return err
	}
	defer parent.Close()
	un, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: un, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, &oa, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, options, 0, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(handle), name)
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return err
	}
	if actual.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || actual.IsDir() != directory || !os.SameFile(expected, actual) {
		return ErrUnsafe
	}
	if !directory {
		if err = checkSingleLink(f); err != nil {
			return err
		}
	}
	var disposition byte = 1
	return windows.NtSetInformationFile(handle, &status, &disposition, 1, windows.FileDispositionInformation)
}
