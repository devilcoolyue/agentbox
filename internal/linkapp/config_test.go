package linkapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRequiresAtLeastOneRule(t *testing.T) {
	_, _, err := Config{}.Validate()
	if err == nil {
		t.Fatal("empty config should not validate: default-deny with no rules connects nothing")
	}
}

// A port map's target must be reachable without the user also having to add it
// as an allow rule — otherwise every map is a two-step setup that silently
// fails when you forget the second step.
func TestValidateMapImpliesAllow(t *testing.T) {
	cfg := Config{Maps: []string{"3306=10.0.1.5:3306"}}
	wl, maps, err := cfg.Validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(maps) != 1 || maps[0].Port != 3306 {
		t.Fatalf("maps = %+v", maps)
	}
	if !wl.Allowed("10.0.1.5", "3306", nil) {
		t.Error("mapped target should be implicitly whitelisted")
	}
	if wl.Allowed("10.0.1.6", "3306", nil) {
		t.Error("whitelist leaked beyond the mapped target")
	}
}

func TestValidateRejectsMalformedMap(t *testing.T) {
	_, _, err := Config{Allow: []string{"10.0.0.0/8"}, Maps: []string{"3306=nohost"}}.Validate()
	if err == nil {
		t.Fatal("want error for target without a port")
	}
}

func TestPaired(t *testing.T) {
	if (Config{Server: "https://a", User: "u"}).Paired() {
		t.Error("a config without a token is not paired")
	}
	if !(Config{Server: "https://a", User: "u", Token: "t"}).Paired() {
		t.Error("full credentials should count as paired")
	}
}

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	t.Setenv("ABOX_LINK_HOME", t.TempDir())

	want := Config{
		Server: "https://box.example.com", User: "alice", Token: "tok",
		Allow: []string{"192.168.1.0/24"}, Maps: []string{"3306=10.0.1.5:3306"},
		AutoConnect: true,
	}
	if err := SaveConfig(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Server != want.Server || got.Token != want.Token || !got.AutoConnect ||
		len(got.Allow) != 1 || len(got.Maps) != 1 {
		t.Fatalf("round trip lost data: %+v", got)
	}

	// The file holds a session token, so it must not be group/world readable.
	fi, err := os.Stat(filepath.Join(ConfigDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestLoadMissingConfigIsNotAnError(t *testing.T) {
	t.Setenv("ABOX_LINK_HOME", filepath.Join(t.TempDir(), "nope"))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("first run should not error: %v", err)
	}
	if cfg.Paired() {
		t.Error("a missing config should read as unpaired")
	}
}

func TestLoadCorruptConfigReportsClearly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ABOX_LINK_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("want a corruption error, got %v", err)
	}
}

func TestDefaultTransparentAndSavedCompatibility(t *testing.T) {
	t.Setenv("ABOX_LINK_HOME", t.TempDir())
	cfg, err := LoadConfig()
	if err != nil || !cfg.Transparent {
		t.Fatalf("new config: %+v %v", cfg, err)
	}
	if err = os.WriteFile(configPath(), []byte(`{"server":"https://fixture.invalid","allow":["db.corp:443"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig()
	if err != nil || !cfg.Transparent {
		t.Fatal("missing mode should use new default", err)
	}
	cfg.Transparent = false
	if err = SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig()
	if err != nil || cfg.Transparent {
		t.Fatal("explicit compatibility selection lost", err)
	}
}
