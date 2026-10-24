package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbox/internal/announce"
)

func TestAnnounceCommandReleaseShowClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"private-secret","data_dir":"data","timezone":"UTC","accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	run := func(args ...string) (*announce.Announcement, error) {
		var out bytes.Buffer
		if err := announceCommand(append([]string{"--config", path}, args...), &out, now); err != nil {
			return nil, err
		}
		var v struct{ Announcement *announce.Announcement }
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			t.Fatalf("output %q: %v", out.String(), err)
		}
		return v.Announcement, nil
	}
	for _, bad := range [][]string{
		{"--release", "--clear"},
		{"--notice", "--in", "5m"},
		{"--release", "--in", "5m", "--at", "2026-10-24T13:00:00Z"},
		{"--release", "--in", "-5m"},
		{"--release", "--at", "tomorrow"},
		{"--notice"},
		{"extra"},
	} {
		if _, err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	a, err := run("--release", "--in", "5m", "--duration", "2", "--message", "升级", "--from-version", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != announce.KindRelease || a.StartsAt != now.Add(5*time.Minute).UnixMilli() || a.DurationMin != 2 || a.FromVersion != "v1" || a.PostedBy != "release" {
		t.Fatalf("release = %+v", a)
	}
	if shown, err := run(); err != nil || shown == nil || shown.ID != a.ID {
		t.Fatalf("show = %+v, %v", shown, err)
	}
	if cleared, err := run("--clear"); err != nil || cleared != nil {
		t.Fatalf("clear = %+v, %v", cleared, err)
	}
	if shown, err := run(); err != nil || shown != nil {
		t.Fatalf("show after clear = %+v, %v", shown, err)
	}
}
