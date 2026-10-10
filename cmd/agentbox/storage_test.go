package main

import (
	"agentbox/internal/config"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStorageConfigOfflineRelocationAndLock(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, next := filepath.Join(root, "old"), filepath.Join(root, "new")
	for _, p := range []string{old, next} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(root, "config.json")
	policy := filepath.Join(root, "policy.json")
	raw, _ := json.Marshal(map[string]any{"auth_token": "fixture-auth", "data_dir": old, "cache_dir": old})
	if err := os.WriteFile(cfg, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte(`{"release_keep":5}`), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(old, "agentbox.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", cfg, "--policy", policy, "--data-dir", next, "--min-free-bytes", "1234"}
	checked := false
	check := func(context.Context, []string) error { checked = true; return nil }
	if err := configureStorage(t.Context(), args, check); err == nil || checked {
		t.Fatal("modified locked data")
	}
	lock.Close()
	if err := configureStorage(t.Context(), args, check); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !checked || loaded.DataDir != next || loaded.CacheDir != next || loaded.GetStoragePolicy().ReleaseKeep != 5 || loaded.GetResources().MinFreeBytes != 1234 {
		t.Fatal("relocation lost settings")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("removed source")
	}
}
