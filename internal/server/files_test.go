package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveFileEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "nested", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeFileEntry(root, "dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dir")); !os.IsNotExist(err) {
		t.Fatalf("directory still exists after removal: %v", err)
	}
	if err := removeFileEntry(root, ""); !errors.Is(err, errFileOpInvalid) {
		t.Fatalf("removing root error = %v, want invalid operation", err)
	}
}

func TestMoveFileEntryAcrossRoots(t *testing.T) {
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "workspace")
	destinationRoot := filepath.Join(base, "shared")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(destinationRoot, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moveFileEntry(sourceRoot, "src/main.go", destinationRoot, "packages")
	if err != nil {
		t.Fatal(err)
	}
	if got != "packages/main.go" {
		t.Fatalf("destination path = %q, want packages/main.go", got)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "src", "main.go")); !os.IsNotExist(err) {
		t.Fatalf("source still exists after move: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(destinationRoot, "packages", "main.go")); err != nil || string(raw) != "package main" {
		t.Fatalf("moved file = %q, %v", raw, err)
	}
}

func TestMoveFileEntryRejectsConflict(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "from"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "to"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "from", "same.txt"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "to", "same.txt"), []byte("destination"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := moveFileEntry(root, "from/same.txt", root, "to")
	if !errors.Is(err, errFileOpConflict) {
		t.Fatalf("move error = %v, want conflict", err)
	}
	if raw, readErr := os.ReadFile(filepath.Join(root, "to", "same.txt")); readErr != nil || string(raw) != "destination" {
		t.Fatalf("destination was overwritten: %q, %v", raw, readErr)
	}
	if raw, readErr := os.ReadFile(filepath.Join(root, "from", "same.txt")); readErr != nil || string(raw) != "source" {
		t.Fatalf("source was changed: %q, %v", raw, readErr)
	}
}

func TestRenameNoReplaceRejectsConflict(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	destination := filepath.Join(root, "destination.txt")
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("destination"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := renameNoReplace(source, destination); !os.IsExist(err) {
		t.Fatalf("rename error = %v, want destination exists", err)
	}
	if raw, err := os.ReadFile(source); err != nil || string(raw) != "source" {
		t.Fatalf("source changed: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(destination); err != nil || string(raw) != "destination" {
		t.Fatalf("destination changed: %q, %v", raw, err)
	}
}

func TestMoveFileEntryRejectsDirectoryDescendant(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "project", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := moveFileEntry(root, "project", root, "project/nested")
	if !errors.Is(err, errFileOpInvalid) {
		t.Fatalf("move error = %v, want invalid operation", err)
	}
}

func TestFileOperationsRejectSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	if err := removeFileEntry(root, "escape/keep.txt"); !errors.Is(err, errFileOpInvalid) {
		t.Fatalf("remove through symlink error = %v, want invalid operation", err)
	}
	if raw, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(raw) != "keep" {
		t.Fatalf("outside file changed: %q, %v", raw, err)
	}
}
