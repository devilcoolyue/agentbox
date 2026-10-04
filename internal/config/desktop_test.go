package config

import (
	"os"
	"testing"
)

func TestDesktopSyncOptInSurvivesUnrelatedSettingsAndFailedSave(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	if c.GetDesktopSyncEnabled() {
		t.Fatal("existing configs must not enable desktop sync")
	}
	enabled := true
	if err := c.ApplySettings(SettingsPatch{DesktopSync: &enabled}); err != nil {
		t.Fatal(err)
	}
	mode := "acceptEdits"
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil || !reloaded.GetDesktopSyncEnabled() {
		t.Fatal("unrelated settings lost desktop opt-in", err)
	}
	saved, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	invalidUpload := int64(-1)
	if err = c.ApplySettings(SettingsPatch{DesktopSync: &disabled, MaxUploadMB: &invalidUpload}); err == nil {
		t.Fatal("invalid update accepted")
	}
	if !c.GetDesktopSyncEnabled() {
		t.Fatal("failed update changed active capability")
	}
	after, _ := os.ReadFile(c.path)
	if string(saved) != string(after) {
		t.Fatal("failed update changed persisted settings")
	}
	if err = c.ApplySettings(SettingsPatch{DesktopSync: &disabled}); err != nil {
		t.Fatal(err)
	}
	reloaded, err = Load(c.path)
	if err != nil || reloaded.GetDesktopSyncEnabled() {
		t.Fatal("explicit opt-out did not persist", err)
	}
}
