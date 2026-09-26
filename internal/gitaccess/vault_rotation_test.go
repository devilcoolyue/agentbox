package gitaccess

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestVaultRotationMixedVersions(t *testing.T) {
	dir := t.TempDir()
	v := &Vault{DataDir: dir}
	aad, plain := []byte("alice/fixture"), []byte("synthetic-rotation-secret")
	legacy, err := v.Seal(plain, aad, true)
	if err != nil {
		t.Fatal(err)
	}
	first, err := v.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	one, err := v.Seal(plain, aad, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := v.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	two, err := v.Seal(plain, aad, false)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || string(one[1:33]) != first || string(two[1:33]) != second || legacy[0] != 1 || one[0] != 2 {
		t.Fatal("incorrect key versions")
	}
	reopened := &Vault{DataDir: dir}
	for _, sealed := range [][]byte{legacy, one, two} {
		got, err := reopened.Open(sealed, aad)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("mixed version decrypt: %v", err)
		}
		if _, err := reopened.Open(sealed, []byte("bob/fixture")); err == nil {
			t.Fatal("wrong identity accepted")
		}
	}
	info, err := os.Stat(filepath.Join(dir, "git-secrets/keyring.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("keyring permissions")
	}
	if id, err := reopened.ActiveKeyID(); err != nil || id != second {
		t.Fatal("active key lost")
	}
	if err := os.Remove(filepath.Join(dir, "git-secrets/master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Open(legacy, aad); err == nil {
		t.Fatal("missing legacy key accepted")
	}
	if _, err := reopened.Open(two, aad); err != nil {
		t.Fatal(err)
	}
}

func TestVaultRotationRejectsDamagedKeyring(t *testing.T) {
	for _, mode := range []string{"corrupt", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			v := &Vault{DataDir: dir}
			if _, err := v.Rotate(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "git-secrets/keyring.json")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := v.Rotate(); err == nil {
				t.Fatal("damaged keyring replaced")
			}
			if _, err := v.Seal([]byte("secret"), nil, true); err == nil {
				t.Fatal("damaged keyring ignored")
			}
		})
	}
}
