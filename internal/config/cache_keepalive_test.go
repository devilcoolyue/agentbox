package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheKeepaliveSettings(t *testing.T) {
	c := writeConfig(t, `{"auth_token":"fixture-secret"}`)
	want := CacheKeepaliveConfig{MaxHours: DefaultKeepaliveHours, MaxHoursLimit: DefaultKeepaliveHoursLimit,
		MinContextTokens: DefaultKeepaliveMinTokens, IntervalMinutes: DefaultKeepaliveInterval, PausePercent: DefaultPausePercent}
	if k := c.GetCacheKeepalive(); k != want {
		t.Fatalf("defaults %+v", k)
	}
	mode := "acceptEdits"
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cache_keepalive") {
		t.Fatal("defaults written out")
	}
	patch := CacheKeepaliveConfig{Disabled: true, MaxHours: 5, MaxHoursLimit: 8, MinContextTokens: 200000, IntervalMinutes: 40, PausePercent: 98}
	if err := c.ApplySettings(SettingsPatch{CacheKeepalive: &patch}); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if k := reloaded.GetCacheKeepalive(); k != patch {
		t.Fatalf("lost after an unrelated save: %+v", k)
	}
	// A default above a lowered limit follows the limit.
	if err := c.ApplySettings(SettingsPatch{CacheKeepalive: &CacheKeepaliveConfig{MaxHoursLimit: 2}}); err != nil {
		t.Fatal(err)
	}
	if k := c.GetCacheKeepalive(); k.MaxHours != 2 || k.MaxHoursLimit != 2 {
		t.Fatalf("default not capped: %+v", k)
	}
	if err := c.ApplySettings(SettingsPatch{CacheKeepalive: &CacheKeepaliveConfig{MaxHours: 4, MaxHoursLimit: 3}}); err == nil {
		t.Fatal("accepted a default above the limit")
	}
	for _, invalid := range []string{`{"max_hours":25}`, `{"max_hours":-1}`, `{"max_hours_limit":25}`, `{"min_context_tokens":-1}`,
		`{"interval_minutes":29}`, `{"interval_minutes":56}`, `{"pause_percent":49}`, `{"pause_percent":100}`, `{"pause_percent":-1}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"auth_token":"fixture-secret","cache_keepalive":`+invalid+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}
