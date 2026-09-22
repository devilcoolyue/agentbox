package config

import (
	"os"
	"reflect"
	"testing"
)

func TestDefaultModelsPersistAndValidate(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	if got := c.GetDefaultModels(); !reflect.DeepEqual(got, initialModels()) {
		t.Fatalf("initial defaults = %v", got)
	}
	// Changing one provider and then an unrelated setting must retain both.
	if err := c.ApplySettings(SettingsPatch{DefaultModels: map[string]string{AgentClaude: "claude-sonnet-5"}}); err != nil {
		t.Fatal(err)
	}
	mode := "acceptEdits"
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{AgentCodex: "gpt-5.5", AgentClaude: "claude-sonnet-5"}
	if got := reloaded.GetDefaultModels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("saved defaults = %v, want %v", got, want)
	}
	before, _ := os.ReadFile(c.path)
	for _, patch := range []SettingsPatch{
		{DefaultModels: map[string]string{AgentCodex: ""}},
		{DefaultModels: map[string]string{AgentCodex: "../bad"}},
		{DefaultModels: map[string]string{"unknown": "gpt-5.5"}},
		{DefaultModels: map[string]string{AgentCodex: "not-in-menu"}},
		{Models: map[string][]ModelOption{AgentCodex: {{ID: "gpt-5.5-codex", Label: "Codex"}}}},
	} {
		if err := c.ApplySettings(patch); err == nil {
			t.Fatalf("accepted invalid defaults/menu: %+v", patch)
		}
		if got := c.GetDefaultModels(); !reflect.DeepEqual(got, want) {
			t.Fatalf("failed patch changed in-memory defaults: %v", got)
		}
		after, _ := os.ReadFile(c.path)
		if string(before) != string(after) {
			t.Fatal("failed patch changed config file")
		}
	}
	copy := c.GetDefaultModels()
	copy[AgentCodex] = "outside-mutation"
	if c.GetDefaultModel(AgentCodex) != "gpt-5.5" {
		t.Fatal("getter leaked mutable defaults")
	}
}

func TestDefaultModelsUpgradeCustomMenu(t *testing.T) {
	c := writeConfig(t, `{"auth_token":"long-test-token","models":{"claude":[{"id":"custom-claude","label":"Custom"}]}}`)
	models := c.GetModels()[AgentClaude]
	if len(models) != 2 || models[0].ID != "custom-claude" || models[1].ID != "claude-opus-5" {
		t.Fatalf("upgrade lost custom options or omitted default: %v", models)
	}
	if err := c.ApplySettings(SettingsPatch{DefaultModels: map[string]string{AgentClaude: "custom-claude"}}); err != nil {
		t.Fatal(err)
	}
}
