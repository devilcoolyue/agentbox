package agent

import (
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/config"
)

func TestSeedingRejectsSymlinkedProviderDirectory(t *testing.T) {
	for _, kind := range []string{config.AgentClaude, config.AgentCodex} {
		t.Run(kind, func(t *testing.T) {
			home, pool, outside := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(pool, "auth.json"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(home, "."+kind)); err != nil {
				t.Fatal(err)
			}
			if err := SeedCredentials(kind, home, pool, os.Getuid(), os.Getgid()); err == nil {
				t.Fatal("credential seed followed link")
			}
			if err := SeedIntranetHint(kind, home, os.Getuid(), os.Getgid()); err == nil {
				t.Fatal("hint seed followed link")
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("outside modified: %v %v", entries, err)
			}
		})
	}
}

func TestSeedingDoesNotOverwriteHardLinkedFile(t *testing.T) {
	home, pool := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(home, ".codex", "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool, "auth.json"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedCredentials(config.AgentCodex, home, pool, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "outside" {
		t.Fatalf("outside overwritten: %q", raw)
	}
}
