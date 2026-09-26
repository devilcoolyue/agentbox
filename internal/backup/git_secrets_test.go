package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
)

func TestGitSecretsSystemBackupRestore(t *testing.T) {
	for _, mode := range []string{"legacy", "mixed-versions", "rotated"} {
		t.Run(mode, func(t *testing.T) {
			testGitSecretsSystemBackupRestore(t, mode)
		})
	}
}
func testGitSecretsSystemBackupRestore(t *testing.T, mode string) {
	cfg, data := fixture(t)
	st, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := store.GitConnection{ID: "fixture-git", Owner: "alice", Label: "Fixture", Provider: "gitlab", BaseURL: "https://git.example.com", Network: gitaccess.NetworkPolicy{Route: "tunnel"}, AuthType: "pat", Username: "oauth2", Enabled: true, ReadOnly: true}
	v := gitaccess.Vault{DataDir: data}
	c.Secret, err = v.Seal([]byte("synthetic-backup-secret"), c.AssociatedData(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.SaveGitConnection(c, 0); err != nil {
		t.Fatal(err)
	}
	if mode != "legacy" {
		if _, err = v.Rotate(); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "rotated" {
		if _, err = st.RekeyGitCredentials(func(item store.GitConnection) ([]byte, error) {
			plain, err := v.Open(item.Secret, item.AssociatedData())
			if err != nil {
				return nil, err
			}
			return v.Seal(plain, item.AssociatedData(), false)
		}); err != nil {
			t.Fatal(err)
		}
	}
	app := config.GitOAuthApp{ID: "fixture-app", Network: gitaccess.NetworkPolicy{Route: "tunnel"}, Provider: "gitlab", BaseURL: "https://git.example.com", ClientID: "fixture-client", RedirectURL: "https://box.example/api/git/oauth/callback", Revision: 1}
	app.Secret, err = v.Seal([]byte("synthetic-app-secret"), app.AssociatedData(), false)
	if err != nil {
		t.Fatal(err)
	}
	configRaw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var configData map[string]any
	if err = json.Unmarshal(configRaw, &configData); err != nil {
		t.Fatal(err)
	}
	configData["git_oauth_apps"] = []config.GitOAuthApp{app}
	configRaw, _ = json.Marshal(configData)
	if err = os.WriteFile(cfg, configRaw, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "system.tar.gz")
	if _, err = Create(t.Context(), Options{Config: cfg, Output: archive}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err = Restore(t.Context(), archive, target); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(filepath.Join(target, "data/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	loaded, err := restored.GitConnection("alice", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	restoredVault := gitaccess.Vault{DataDir: filepath.Join(target, "data")}
	plain, err := restoredVault.Open(loaded.Secret, loaded.AssociatedData())
	if err != nil || string(plain) != "synthetic-backup-secret" {
		t.Fatalf("restored credential: %v", err)
	}
	restoredConfig, err := os.ReadFile(filepath.Join(target, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = checkGitOAuthSecrets(restoredConfig, filepath.Join(target, "data")); err != nil {
		t.Fatalf("restored app secret: %v", err)
	}
	missing := "git-secrets/master.key"
	if mode == "rotated" {
		missing = "git-secrets/keyring.json"
	}
	if err = os.Remove(filepath.Join(data, missing)); err != nil {
		t.Fatal(err)
	}
	if _, err = Create(t.Context(), Options{Config: cfg, Output: filepath.Join(t.TempDir(), "bad.tar.gz")}); err == nil {
		t.Fatal("backup reported success despite missing decryption key")
	}
}
