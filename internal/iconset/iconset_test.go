package iconset

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func fixtureLibrary() *Library {
	set := func(icons string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`{"icons":` + icons + `}`)}
	}
	return NewLibrary(fstest.MapFS{
		"vendor/icons/tabler.json":        set(`{"receipt":["M5 21v-16"],"x":["M18 6l-12 12"]}`),
		"vendor/icons/tabler-filled.json": set(`{"star":["M8 1z"]}`),
		"vendor/icons/lucide.json":        set(`{"puzzle":[["circle",{"cx":"12","cy":"12","r":"3"}]]}`),
	})
}

func TestValidateRejectsUnknownOrMalformedEntries(t *testing.T) {
	lib := fixtureLibrary()
	if err := lib.Validate(map[string]string{"nav.usage": "tabler:receipt", "close": "lucide:puzzle", "x": "tabler-filled:star"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{
		{"Nav.Usage": "tabler:receipt"},
		{"nav..usage": "tabler:receipt"},
		{"nav.usage": "tabler:missing"},
		{"nav.usage": "other:receipt"},
		{"nav.usage": "tabler:<svg>"},
		{"nav.usage": ""},
	} {
		var ve *ValidationError
		if err := lib.Validate(bad); !errors.As(err, &ve) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
	many := map[string]string{}
	for i := 0; i <= MaxOverrides; i++ {
		many[fmt.Sprintf("slot%d", i)] = "tabler:x"
	}
	if lib.Validate(many) == nil {
		t.Fatal("override limit not enforced")
	}
}

func TestPutGetRoundTripAndShapes(t *testing.T) {
	dir := t.TempDir()
	svc := New(dir, fixtureLibrary())
	cfg, err := svc.Get()
	if err != nil || len(cfg.Overrides) != 0 {
		t.Fatalf("empty config: %+v %v", cfg, err)
	}
	if _, err := svc.Put(map[string]string{"nav.usage": "tabler:missing"}, "root"); err == nil {
		t.Fatal("unknown icon saved")
	}
	if _, err := svc.Put(map[string]string{"nav.usage": "tabler:receipt", "close": "lucide:puzzle"}, "root"); err != nil {
		t.Fatal(err)
	}
	cfg, err = svc.Get()
	if err != nil || cfg.Overrides["nav.usage"] != "tabler:receipt" || cfg.UpdatedBy != "root" || cfg.UpdatedAt == 0 {
		t.Fatalf("round trip: %+v %v", cfg, err)
	}
	shapes, err := svc.Library().Shapes(cfg)
	if err != nil || len(shapes) != 2 || string(shapes["lucide:puzzle"]) != `[["circle",{"cx":"12","cy":"12","r":"3"}]]` {
		t.Fatalf("shapes: %s %v", shapes, err)
	}
	info, err := os.Stat(filepath.Join(dir, file))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
}

func TestGetDropsHandEditedInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(Config{Version: 1, Overrides: map[string]string{"nav.usage": "tabler:receipt", "close": "tabler:gone", "BAD": "tabler:x"}})
	if err := os.WriteFile(filepath.Join(dir, file), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := New(dir, fixtureLibrary()).Get()
	if err != nil || len(cfg.Overrides) != 1 || cfg.Overrides["nav.usage"] != "tabler:receipt" {
		t.Fatalf("got %+v %v", cfg, err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(`{"version":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := New(dir, fixtureLibrary()).Get(); err == nil || len(cfg.Overrides) != 0 {
		t.Fatalf("future version accepted: %+v %v", cfg, err)
	}
}
