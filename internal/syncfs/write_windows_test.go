package syncfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsLockedDestinationKeepsOriginalBytes(t *testing.T) {
	r, dir := fixtureRoot(t)
	w := Writer{Root: r}
	file := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(file, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	// Permit reading for hashing/recovery but deny rename/delete while an editor
	// holds the destination. The implementation must not unlink it as fallback.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := fileEntry("old")
	new := fileEntry("new")
	_, replaceErr := w.Replace(t.Context(), "locked.txt", &old, new, bytes.NewBufferString("new"))
	windows.CloseHandle(handle)
	if replaceErr == nil {
		t.Fatal("replaced file denied by Windows sharing mode")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "old" {
		t.Fatalf("locked target changed: %q %v", data, err)
	}
	if _, err = w.Replace(t.Context(), "locked.txt", &old, new, bytes.NewBufferString("new")); err != nil {
		t.Fatal("retry after unlock", err)
	}
}
