package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoragePolicyPersistenceAndValidation(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	p := StoragePolicy{Mount: filepath.Dir(c.DataDir), UUID: "11111111-1111-1111-1111-111111111111", UserBytes: 1 << 30, Users: map[string]int64{"alice": 2 << 30}, ReleaseKeep: 5}
	if err := c.SetStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	p.Users["alice"] = 1
	got := c.GetStoragePolicy()
	if got.Users["alice"] != 2<<30 {
		t.Fatal("caller mutated policy")
	}
	tz := "UTC"
	if err := c.ApplySettings(SettingsPatch{TimeZone: &tz}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.GetStoragePolicy(), got) {
		t.Fatal("policy lost on mutation")
	}
	for _, bad := range []StoragePolicy{{UserBytes: 1 << 30}, {ReleaseKeep: 1}, {Mount: "/", UUID: p.UUID, UserBytes: 1 << 30}, {Mount: "/elsewhere", UUID: p.UUID, UserBytes: 1 << 30}, {CacheMaxAgeDays: -1}} {
		if err := c.SetStoragePolicy(bad); err == nil {
			t.Fatalf("accepted invalid %+v", bad)
		}
	}
}
