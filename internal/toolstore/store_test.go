package toolstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserIdentityAndPinnedMountRoots(t *testing.T) {
	root := t.TempDir()
	alice := OwnerKey("instance", "alice", "created-1")
	bob := OwnerKey("instance", "bob", "created-1")
	again := OwnerKey("instance", "alice", "created-2")
	if alice == bob || alice == again || alice == OwnerKey("other", "alice", "created-1") {
		t.Fatal("cache identity reused")
	}
	a, err := Prepare(root, alice, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Prepare(root, bob, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	if a.Public != b.Public || a.Cache == b.Cache {
		t.Fatal("wrong sharing boundary")
	}
	if err = os.RemoveAll(a.Cache); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, a.Cache); err != nil {
		t.Fatal(err)
	}
	if _, err = Prepare(root, alice, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("followed substituted mount root")
	}
	if _, err = Prepare(root, "../bob", os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("accepted cache traversal")
	}
}

func TestSDKPublicationAndArchitecture(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "public/go/linux-amd64/go1.26.9")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin/go"), []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := GoBin(root, "amd64", "go1.26.9"); err == nil {
		t.Fatal("used unpublished SDK")
	}
	if err := os.WriteFile(filepath.Join(dir, ".agentbox-tool.json"), []byte(`{"format":1,"kind":"go","version":"go1.26.9","platform":"linux-amd64"}`), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := GoBin(root, "amd64", "go1.26.9")
	if err != nil || got != PublicMount+"/go/linux-amd64/go1.26.9/bin" {
		t.Fatal(got, err)
	}
	if _, err = GoBin(root, "arm64", "go1.26.9"); err == nil {
		t.Fatal("mixed SDK architectures")
	}
}
