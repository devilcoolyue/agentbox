package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/safefs"
	"github.com/pelletier/go-toml/v2"
)

func TestCollaborationHomePreservesConnectionWithoutInheritedTools(t *testing.T) {
	home, pool := t.TempDir(), t.TempDir()
	uid, gid := os.Getuid(), os.Getgid()
	config := `profile="active"
model_provider="old"
notify=["/bad"]
[profiles.active]
model_provider="relay"
model_reasoning_effort="high"
[model_providers.relay]
name="Fixture"
base_url="https://example.com/v1"
wire_api="responses"
env_key="FIXTURE_KEY"
[mcp_servers.evil]
command="/bad"
[hooks]
start="bad"
`
	if err := os.WriteFile(filepath.Join(pool, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool, "auth.json"), []byte(`{"fixture":"login"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool, "hook.sh"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SeedCollaborationHome("codex", home, pool, "selected-model", uid, gid); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err = toml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if values["model"] != "selected-model" || values["model_provider"] != "relay" || values["model_reasoning_effort"] != "high" {
		t.Fatal(values)
	}
	for _, key := range []string{"mcp_servers", "notify", "hooks", "profiles", "profile"} {
		if _, exists := values[key]; exists {
			t.Fatal("inherited executable config", key)
		}
	}
	if _, err = os.Stat(filepath.Join(home, ".codex/hook.sh")); !os.IsNotExist(err) {
		t.Fatal("copied arbitrary account file")
	}
	if err = SeedCollaborationHome("claude", home, "", "claude-fixture", uid, gid); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	values = map[string]any{}
	if err = json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values["disableAllHooks"] != true {
		t.Fatal(values)
	}
}

func TestCollaborationProjectConfigAndCredentialSymlinksAreRejected(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".codex/config.toml"), []byte("notify=[]"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := safefs.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = ValidateCollaborationProject(r); err == nil {
		t.Fatal("unreviewed project settings accepted")
	}
	home, pool, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err = os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(pool, "auth.json"), []byte("login"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "keep")
	if err = os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, filepath.Join(home, ".codex/auth.json")); err != nil {
		t.Fatal(err)
	}
	if err = SeedCollaborationHome("codex", home, pool, "fixture", os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("followed credential symlink")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "keep" {
		t.Fatal("external file changed", err)
	}
}

func TestCollaborationProjectNestedExtensionsRejected(t *testing.T) {
	for _, name := range []string{"packages/app/.mcp.json", "src/.claude/settings.local.json", "nested/.codex/config.toml", "nested/.agents/skills/tool/SKILL.md"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := safefs.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err = ValidateCollaborationProject(root); err == nil {
				t.Fatal("nested executable extension accepted")
			}
		})
	}
}
