package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/config"
)

func TestBuiltinTemplateFollowsTunnelSwitch(t *testing.T) {
	root := t.TempDir()
	off := New(&config.Config{DataDir: root}, nil, nil, nil, nil)
	if dirs := off.templateDirs("alice"); len(dirs) != 2 || dirs[0] != off.HomeTemplateDir() {
		t.Fatalf("tunnel off: %v", dirs)
	}
	if _, err := os.Stat(off.BuiltinTemplateDir()); !os.IsNotExist(err) {
		t.Fatalf("built-in layer written with the tunnel off: %v", err)
	}

	on := New(&config.Config{DataDir: root, Tunnel: config.TunnelConfig{Enabled: true}}, nil, nil, nil, nil)
	dirs := on.templateDirs("alice")
	if len(dirs) != 3 || dirs[0] != on.BuiltinTemplateDir() || dirs[2] != on.UserTemplateDir("alice") {
		t.Fatalf("tunnel on: %v", dirs)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], ".local", "bin", "abox-remote")); err != nil {
		t.Fatal(err)
	}
}
