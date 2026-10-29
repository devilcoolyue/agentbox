package config

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestTurnTimeoutSettings(t *testing.T) {
	c := writeConfig(t, `{"auth_token":"fixture-secret"}`)
	if c.GetTurnTimeout() != 2*time.Hour {
		t.Fatal("a missing setting must default to two hours")
	}
	minutes := int64(240)
	if err := c.ApplySettings(SettingsPatch{TurnTimeoutMin: &minutes}); err != nil {
		t.Fatal(err)
	}
	mode := "acceptEdits"
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil || reloaded.GetTurnTimeout() != 4*time.Hour {
		t.Fatalf("setting lost after unrelated save/reload: %v", err)
	}
	before, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []int64{-1, 4, 1441} {
		if err := c.ApplySettings(SettingsPatch{TurnTimeoutMin: &invalid}); err == nil {
			t.Fatalf("accepted invalid limit: %d", invalid)
		}
		after, err := os.ReadFile(c.path)
		if err != nil || !bytes.Equal(before, after) || c.GetTurnTimeout() != 4*time.Hour {
			t.Fatalf("invalid patch changed disk or memory: %v", err)
		}
	}
	for _, bound := range []int64{MinTurnTimeoutMin, MaxTurnTimeoutMin} {
		if err := c.ApplySettings(SettingsPatch{TurnTimeoutMin: &bound}); err != nil {
			t.Fatalf("valid boundary %d rejected: %v", bound, err)
		}
	}
}

func TestMaxParallelChatsSettings(t *testing.T) {
	c := writeConfig(t, `{"auth_token":"fixture-secret"}`)
	if c.GetMaxParallelChats() != DefaultMaxParallelChats {
		t.Fatal("a missing setting must default to three threads")
	}
	n := int64(6)
	if err := c.ApplySettings(SettingsPatch{MaxParallelChats: &n}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil || reloaded.GetMaxParallelChats() != 6 {
		t.Fatalf("setting lost after reload: %v", err)
	}
	for _, invalid := range []int64{-1, MaxParallelChatsLimit + 1} {
		if err := c.ApplySettings(SettingsPatch{MaxParallelChats: &invalid}); err == nil {
			t.Fatalf("accepted invalid limit: %d", invalid)
		}
		if c.GetMaxParallelChats() != 6 {
			t.Fatal("invalid patch changed memory")
		}
	}
	for _, bound := range []int64{1, MaxParallelChatsLimit} {
		if err := c.ApplySettings(SettingsPatch{MaxParallelChats: &bound}); err != nil {
			t.Fatalf("valid boundary %d rejected: %v", bound, err)
		}
	}
}
