package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollaborationEnableSurvivesUnrelatedSettingsMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"fixture-token-long-enough","data_dir":"data","accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.GetCollaborationEnabled() {
		t.Fatal("default must be disabled")
	}
	if err = c.SetCollaborationEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err = c.mutate(func(w *Config) error { w.MaxUploadMB = 17; return nil }); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil || !c.GetCollaborationEnabled() {
		t.Fatal("setting dropped on persist", err)
	}
}
