package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIndependentPathsPersistAcrossMutation(t *testing.T) {
	c := writeConfig(t, `{"auth_token":"fixture-token-long-enough","data_dir":"state","cache_dir":"cache","resources":{"max_running":3,"max_running_per_user":1,"min_free_bytes":123}}`)
	if c.CacheDir != filepath.Join(filepath.Dir(c.Path()), "cache") {
		t.Fatal(c.CacheDir)
	}
	tz := "UTC"
	if err := c.ApplySettings(SettingsPatch{TimeZone: &tz}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.Path())
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	if cfg["cache_dir"] != "cache" || cfg["data_dir"] != "state" {
		t.Fatal(cfg)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GetResources() != c.GetResources() {
		t.Fatal("limits lost on save")
	}
	bad := ResourceLimits{MaxRunning: -1}
	if err := c.ApplySettings(SettingsPatch{Resources: &bad}); err == nil {
		t.Fatal("negative limit accepted")
	}
	old := writeConfig(t, minimalConfig)
	if old.CacheDir != old.DataDir {
		t.Fatal("legacy cache moved")
	}
}
