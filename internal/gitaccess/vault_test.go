package gitaccess

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestVaultAuthenticatedStorageAndMissingKey(t *testing.T) {
	dir := t.TempDir()
	v := Vault{DataDir: dir}
	aad := []byte("owner/id/host")
	secret := []byte("synthetic-test-credential")
	sealed, err := v.Seal(secret, aad, true)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("plaintext secret persisted")
	}
	info, err := os.Stat(filepath.Join(dir, "git-secrets/master.key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions: %v %v", info, err)
	}
	reopened := Vault{DataDir: dir}
	plain, err := reopened.Open(sealed, aad)
	if err != nil || !bytes.Equal(plain, secret) {
		t.Fatalf("decrypt: %q %v", plain, err)
	}
	if _, err = v.Open(sealed, []byte("different-owner")); err == nil {
		t.Fatal("authenticated metadata ignored")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err = v.Open(sealed, aad); err == nil {
		t.Fatal("tampering accepted")
	}
	if err = os.Remove(filepath.Join(dir, "git-secrets/master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Seal(secret, aad, false); err == nil {
		t.Fatal("missing key silently replaced")
	}
	if _, err = v.Open(sealed, aad); err == nil {
		t.Fatal("missing key accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.WriteFile(outside, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "git-secrets/master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Seal(secret, aad, true); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestEndpointValidation(t *testing.T) {
	for _, raw := range []string{"http://git.example.com", "https://user:token@git.example.com", "https://git.example.com/a/../b", "https://git.example.com/a%2fb", "https://git.example.com?token=x", "https://git.example.com#x", "https://git.example.com\n", "https://git.example.com/?", "file:///tmp/git"} {
		if _, err := BaseURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	base, err := BaseURL("https://GIT.EXAMPLE.COM:443/gitlab/")
	if err != nil || base != "https://git.example.com/gitlab" {
		t.Fatalf("normalize %q %v", base, err)
	}
	if _, err = RepositoryURL("https://git.example.com/gitlab/team/repo.git", base); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://git.example.com/gitlab-other/repo", "https://evil.example.com/gitlab/repo", "https://git.example.com/gitlab"} {
		if _, err = RepositoryURL(raw, base); err == nil {
			t.Errorf("outside base accepted %q", raw)
		}
	}
}
