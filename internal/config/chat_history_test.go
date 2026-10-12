package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestChatHistoryTimeoutSettings(t *testing.T) {
	c := writeConfig(t, `{"auth_token":"fixture-secret"}`)
	if c.GetChatHistoryTimeoutSec() != 120 || c.GetMaxUploadMB() != 512 {
		t.Fatal("missing settings must default to 120 seconds and 512 MB")
	}
	seconds, upload := int64(240), int64(128)
	if err := c.ApplySettings(SettingsPatch{ChatHistoryTimeoutSec: &seconds, MaxUploadMB: &upload}); err != nil {
		t.Fatal(err)
	}
	mode := "acceptEdits"
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil || reloaded.GetChatHistoryTimeoutSec() != seconds || reloaded.GetMaxUploadMB() != upload {
		t.Fatalf("settings lost after unrelated save/reload: %v", err)
	}
	before, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []int64{-1, 0, 3601} {
		if err := c.ApplySettings(SettingsPatch{ChatHistoryTimeoutSec: &invalid}); err == nil {
			t.Fatalf("accepted invalid timeout: %d", invalid)
		}
		after, err := os.ReadFile(c.path)
		if err != nil || !bytes.Equal(before, after) || c.GetChatHistoryTimeoutSec() != seconds {
			t.Fatalf("invalid patch changed disk or memory: %v", err)
		}
	}
	for _, bound := range []int64{1, 3600} {
		if err := c.ApplySettings(SettingsPatch{ChatHistoryTimeoutSec: &bound}); err != nil {
			t.Fatalf("valid boundary %d rejected: %v", bound, err)
		}
	}
	// A failed write must not expose the unsaved value to /me or settings.
	c.path = filepath.Join(t.TempDir(), "missing", "config.json")
	if err := c.ApplySettings(SettingsPatch{ChatHistoryTimeoutSec: &seconds}); err == nil || c.GetChatHistoryTimeoutSec() != 3600 {
		t.Fatalf("failed save changed runtime timeout: %v", err)
	}
}
