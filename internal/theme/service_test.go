package theme

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func manifest(id, name string) Manifest {
	return Manifest{Format: FormatVersion, ID: id, Name: name, Base: "graphite", Dark: map[string]string{"--bg": "#101010"}}
}

func TestServiceScopesLimitsAndRemoval(t *testing.T) {
	data := t.TempDir()
	s := New(data)
	if list, err := s.List(""); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	if _, err := s.Put("", manifest("zeta", "Zeta")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("", manifest("alpha", "alpha")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("alice", manifest("mine", "Mine")); err != nil {
		t.Fatal(err)
	}
	site, _ := s.List("")
	if len(site) != 2 || site[0].Manifest.ID != "alpha" || site[1].Manifest.ID != "zeta" || site[0].UpdatedAt.IsZero() {
		t.Fatalf("site list: %+v", site)
	}
	if bob, _ := s.List("bob"); len(bob) != 0 {
		t.Fatal("personal themes leaked across users", bob)
	}
	for _, p := range []string{"themes.json", "users/alice/themes.json"} {
		info, err := os.Stat(filepath.Join(data, p))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal(p, info, err)
		}
	}

	// Replacing an existing ID never counts against the limit.
	for i := 1; i < MaxUser; i++ {
		if _, err := s.Put("alice", manifest(fmt.Sprintf("t%d", i), "T")); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := s.Put("alice", manifest("one-more", "T")); !errors.Is(err, ErrLimit) {
		t.Fatal("limit not enforced", err)
	}
	replaced := manifest("mine", "Renamed")
	if _, err := s.Put("alice", replaced); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete("", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Delete("", "zeta"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUser("alice"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.List("alice"); len(list) != 0 {
		t.Fatal("removed user keeps themes", list)
	}
	if err := s.RemoveUser("alice"); err != nil {
		t.Fatal("removing again must be idempotent", err)
	}
	for _, bad := range []string{"..", "a/b", ""} {
		if _, err := s.List(bad); bad != "" && err == nil {
			t.Fatal("bad owner accepted", bad)
		}
	}
}

func TestServiceSkipsTamperedEntries(t *testing.T) {
	data := t.TempDir()
	raw := `{"version":1,"themes":{
		"ok":{"manifest":{"agentbox_theme":1,"id":"ok","name":"OK","base":"amber","dark":{"--bg":"#000"}},"updated_at":"2026-01-01T00:00:00Z"},
		"evil":{"manifest":{"agentbox_theme":1,"id":"evil","name":"Evil","base":"amber","dark":{"--bg":"url(https://evil.example)"}},"updated_at":"2026-01-01T00:00:00Z"},
		"moved":{"manifest":{"agentbox_theme":1,"id":"other","name":"Moved","base":"amber"},"updated_at":"2026-01-01T00:00:00Z"}
	}}`
	if err := os.WriteFile(filepath.Join(data, "themes.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := New(data).List("")
	if err != nil || len(list) != 1 || list[0].Manifest.ID != "ok" {
		t.Fatal(list, err)
	}
	if err := os.WriteFile(filepath.Join(data, "themes.json"), []byte(`{"version":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(data).List(""); err == nil {
		t.Fatal("unknown file version accepted")
	}
}

func TestServiceRefusesSymlinkedUserDirectory(t *testing.T) {
	data, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "users"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "users", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(data).Put("alice", manifest("x", "X")); err == nil {
		t.Fatal("wrote through a symlinked user directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "themes.json")); !os.IsNotExist(err) {
		t.Fatal("file escaped the data directory", err)
	}
}
