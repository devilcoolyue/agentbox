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
	for _, raw := range []string{"https://git.example.com/gitlab-other/repo", "https://evil.example.com/gitlab/repo", "https://git.example.com/gitlab", "http://git.example.com/gitlab/team/repo.git"} {
		if _, err = RepositoryURL(raw, base); err == nil {
			t.Errorf("outside base accepted %q", raw)
		}
	}
}

func TestHTTPEndpointRequiresTunnel(t *testing.T) {
	for _, p := range []NetworkPolicy{{}, {Route: "direct"}, {Route: "tunnel", CAPEM: "x"}} {
		if _, err := ConnectionBaseURL("http://192.168.2.66:7077/gitlab", p); err == nil {
			t.Errorf("HTTP accepted with %+v", p)
		}
	}
	tunnel := NetworkPolicy{Route: "tunnel"}
	for _, raw := range []string{"http://user:pw@git.lan", "http://git.lan?x=1", "http://git.lan/a/../b", "ftp://git.lan"} {
		if _, err := ConnectionBaseURL(raw, tunnel); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if got, err := ConnectionBaseURL("https://git.example.com:443", NetworkPolicy{}); err != nil || got != "https://git.example.com" {
		t.Fatalf("https direct: %q %v", got, err)
	}
	base, err := ConnectionBaseURL("http://Git.LAN:80/gitlab/", tunnel)
	if err != nil || base != "http://git.lan/gitlab" {
		t.Fatalf("normalize %q %v", base, err)
	}
	if got, err := RepositoryURL("http://git.lan:80/gitlab/team/sub/repo.git", base); err != nil || got != "http://git.lan/gitlab/team/sub/repo.git" {
		t.Fatalf("repository %q %v", got, err)
	}
	if _, err = RepositoryURL("https://git.lan/gitlab/team/repo.git", base); err == nil {
		t.Error("scheme change accepted")
	}
	base, err = ConnectionBaseURL("http://192.168.2.66:7077/gitlab", tunnel)
	if err != nil || base != "http://192.168.2.66:7077/gitlab" {
		t.Fatalf("intranet %q %v", base, err)
	}
}
