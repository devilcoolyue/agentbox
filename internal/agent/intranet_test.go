package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
)

func TestSeedIntranetHint(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "CLAUDE.md")

	// First seed into an empty home.
	if err := SeedIntranetHint(config.AgentClaude, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected %s written: %v", path, err)
	}
	if !strings.Contains(string(first), "AGENTBOX_INTRANET_PROXY") {
		t.Fatal("hint body missing")
	}

	// Re-seed: must be idempotent (no duplicate block).
	if err := SeedIntranetHint(config.AgentClaude, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if strings.Count(string(second), intranetHintBegin) != 1 {
		t.Fatalf("expected exactly one managed block, got %d", strings.Count(string(second), intranetHintBegin))
	}

	// User content around the block must survive a re-seed.
	withUser := "# My notes\nkeep me\n\n" + string(second) + "\ntrailing user line\n"
	if err := os.WriteFile(path, []byte(withUser), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedIntranetHint(config.AgentClaude, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(path)
	fs := string(final)
	if !strings.Contains(fs, "keep me") || !strings.Contains(fs, "trailing user line") {
		t.Fatal("user content was clobbered")
	}
	if strings.Count(fs, intranetHintBegin) != 1 {
		t.Fatal("managed block duplicated after re-seed with surrounding user content")
	}
}

func TestSeedIntranetHintCodexPath(t *testing.T) {
	home := t.TempDir()
	if err := SeedIntranetHint(config.AgentCodex, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); err != nil {
		t.Fatalf("codex hint not written: %v", err)
	}
}

func TestTransparentHintReplacesLegacyGuidance(t *testing.T) {
	home := t.TempDir()
	if err := SeedIntranetHint("codex", home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := SeedTransparentHint("codex", home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "勿尝试代理") || !strings.Contains(string(raw), "直接使用原地址") {
		t.Fatal(string(raw))
	}
}
