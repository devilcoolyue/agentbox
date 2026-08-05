package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAt(t *testing.T, path, body string, perm os.FileMode, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func seedTemplate(t *testing.T, home string, tmpl ...string) {
	t.Helper()
	if err := SeedHomeTemplate(home, os.Getuid(), os.Getgid(), tmpl...); err != nil {
		t.Fatal(err)
	}
}

func TestSeedHomeTemplate(t *testing.T) {
	tmpl, home := t.TempDir(), t.TempDir()
	old := time.Now().Add(-2 * time.Hour)

	skill := filepath.Join(tmpl, ".claude", "skills", "deploy", "SKILL.md")
	hook := filepath.Join(tmpl, ".claude", "hooks", "notify.sh")
	writeAt(t, skill, "# deploy\n", 0o644, old)
	writeAt(t, hook, "#!/bin/sh\n", 0o755, old)
	if err := os.Symlink("/shared/big-skill", filepath.Join(tmpl, ".claude", "skills", "big")); err != nil {
		t.Fatal(err)
	}

	seedTemplate(t, home, tmpl)

	if body, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "deploy", "SKILL.md")); err != nil {
		t.Fatalf("skill not seeded: %v", err)
	} else if string(body) != "# deploy\n" {
		t.Fatalf("skill body = %q", body)
	}
	fi, err := os.Stat(filepath.Join(home, ".claude", "hooks", "notify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable bit lost: %v", fi.Mode())
	}
	// Symlinks are recreated, not dereferenced: the dangling target is the point.
	link, err := os.Readlink(filepath.Join(home, ".claude", "skills", "big"))
	if err != nil {
		t.Fatalf("symlink not seeded: %v", err)
	}
	if link != "/shared/big-skill" {
		t.Fatalf("symlink target = %q", link)
	}
}

func TestSeedHomeTemplateNewerWins(t *testing.T) {
	tmpl, home := t.TempDir(), t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	src := filepath.Join(tmpl, ".claude", "settings.json")
	dst := filepath.Join(home, ".claude", "settings.json")

	writeAt(t, src, "v1\n", 0o644, old)
	seedTemplate(t, home, tmpl)

	// Edited inside the container (newer than the template) → kept.
	writeAt(t, dst, "edited-in-session\n", 0o644, time.Now().Add(-time.Hour))
	seedTemplate(t, home, tmpl)
	if body, _ := os.ReadFile(dst); string(body) != "edited-in-session\n" {
		t.Fatalf("in-session edit clobbered: %q", body)
	}

	// Template moves ahead → the update propagates to the existing session.
	writeAt(t, src, "v2\n", 0o644, time.Now())
	seedTemplate(t, home, tmpl)
	if body, _ := os.ReadFile(dst); string(body) != "v2\n" {
		t.Fatalf("template update not propagated: %q", body)
	}

	// Steady state: a re-seed right after must not rewrite the file.
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	seedTemplate(t, home, tmpl)
	after, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("re-seed rewrote an already-current file")
	}
}

// The per-user template must win over the server-wide one even when its files
// are older — layering is resolved before anything is compared to the session.
func TestSeedHomeTemplateUserLayerWins(t *testing.T) {
	global, user, home := t.TempDir(), t.TempDir(), t.TempDir()
	newer, older := time.Now(), time.Now().Add(-24*time.Hour)

	writeAt(t, filepath.Join(global, ".claude", "settings.json"), "global\n", 0o644, newer)
	writeAt(t, filepath.Join(global, ".claude", "skills", "shared", "SKILL.md"), "shared\n", 0o644, newer)
	writeAt(t, filepath.Join(user, ".claude", "settings.json"), "user\n", 0o644, older)
	writeAt(t, filepath.Join(user, ".claude", "skills", "mine", "SKILL.md"), "mine\n", 0o644, older)

	seedTemplate(t, home, global, user)

	if body, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); string(body) != "user\n" {
		t.Fatalf("user layer lost to the global one: %q", body)
	}
	// Non-overlapping entries from both layers still land.
	for _, rel := range []string{".claude/skills/shared/SKILL.md", ".claude/skills/mine/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
	}
}

// A later layer replacing a directory with a file must not leave the earlier
// layer's children behind (they'd have no parent directory to live in).
func TestSeedHomeTemplateLayerTypeChange(t *testing.T) {
	global, user, home := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now()

	writeAt(t, filepath.Join(global, ".claude", "skills", "x", "SKILL.md"), "dir\n", 0o644, now)
	writeAt(t, filepath.Join(user, ".claude", "skills", "x"), "file\n", 0o644, now)

	seedTemplate(t, home, global, user)

	fi, err := os.Lstat(filepath.Join(home, ".claude", "skills", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		t.Fatal("user layer's file did not replace the global layer's directory")
	}
}

func TestSeedHomeTemplateMissingOrEmpty(t *testing.T) {
	home := t.TempDir()
	if err := SeedHomeTemplate(home, os.Getuid(), os.Getgid(), ""); err != nil {
		t.Fatalf("empty path should be a no-op: %v", err)
	}
	if err := SeedHomeTemplate(home, os.Getuid(), os.Getgid(), filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("absent template should be a no-op: %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("home touched by a no-op seed: %v", entries)
	}

	file := filepath.Join(t.TempDir(), "file")
	writeAt(t, file, "x", 0o644, time.Now())
	if err := SeedHomeTemplate(home, os.Getuid(), os.Getgid(), file); err == nil {
		t.Fatal("expected an error when the template path is a regular file")
	}
}
