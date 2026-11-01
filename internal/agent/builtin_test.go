package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Changing anything under builtin/ must come with a new BuiltinTemplateTime,
// or sessions that already have the old copy never receive the change.
func TestBuiltinTemplateDigest(t *testing.T) {
	if got := BuiltinTemplateDigest(); got != builtinTemplateDigest {
		t.Fatalf("built-in template changed: bump BuiltinTemplateTime and set builtinTemplateDigest = %q", got)
	}
}

func TestBuiltinTemplateLayersUnderOperatorTemplates(t *testing.T) {
	data := t.TempDir()
	builtin := filepath.Join(data, "builtin-template")
	if err := WriteBuiltinTemplate(builtin); err != nil {
		t.Fatal(err)
	}
	// Rewriting (every server start) replaces the tree in place.
	if err := WriteBuiltinTemplate(builtin); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(builtin, ".local", "bin", "abox-remote")
	fi, err := os.Stat(tool)
	if err != nil || fi.Mode().Perm() != 0o755 || !fi.ModTime().Equal(BuiltinTemplateTime) {
		t.Fatalf("tool = %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(builtin, ".claude", "skills", "remote-host", "SKILL.md")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("skill = %v, %v", fi, err)
	}

	server, home := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(server, ".claude", "skills", "remote-host", "SKILL.md"), "# operator copy\n", 0o644, BuiltinTemplateTime.Add(-time.Hour))
	seedTemplate(t, home, builtin, server)
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "skills", "remote-host", "SKILL.md")); string(b) != "# operator copy\n" {
		t.Fatalf("server template must override the built-in layer, got %q", b)
	}
	if fi, err := os.Stat(filepath.Join(home, ".local", "bin", "abox-remote")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("seeded tool = %v, %v", fi, err)
	}
}
