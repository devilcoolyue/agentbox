package config

import (
	"path/filepath"
	"testing"
)

func TestToolStoragePersistence(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	p := ToolStorage{Root: filepath.Join(t.TempDir(), "tools"), GoVersion: "go1.26.9"}
	if err := c.SetToolStorage(p); err != nil {
		t.Fatal(err)
	}
	idle := int64(7)
	if err := c.ApplySettings(SettingsPatch{IdleTimeoutMin: &idle}); err != nil {
		t.Fatal(err)
	}
	next, err := Load(c.Path())
	if err != nil || next.GetToolStorage() != p {
		t.Fatal(next, err)
	}
	for _, invalid := range []ToolStorage{{Root: "relative"}, {Root: "/"}, {Root: filepath.Join(c.DataDir, "users/alice/tools")}, {GoVersion: "go1.26.9"}, {Root: p.Root, GoVersion: "latest"}} {
		if err := c.SetToolStorage(invalid); err == nil {
			t.Fatal("accepted invalid tool config", invalid)
		}
	}
}
