package config

import (
	"bytes"
	"testing"
)

func TestGitOAuthAppPersistenceAndImmutability(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	app := GitOAuthApp{ID: "git-fixture", Label: "Company Git", Provider: "gitlab", BaseURL: "https://git.example.com/gitlab", ClientID: "fixture-client", Secret: bytes.Repeat([]byte{1}, 40), RedirectURL: "https://agentbox.example/api/git/oauth/callback", Enabled: true}
	if err := c.SaveGitOAuthApp(app, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(SettingsPatch{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reloaded.GitOAuthApp(app.ID)
	if !ok || saved.Revision != 1 || !bytes.Equal(saved.Secret, app.Secret) {
		t.Fatalf("lost application: %+v", saved)
	}
	copy := c.GitOAuthAppList()
	copy[0].Secret[0] = 9
	actual, _ := c.GitOAuthApp(app.ID)
	if actual.Secret[0] != 1 {
		t.Fatal("snapshot aliases secret buffer")
	}
	saved.ClientID = "other-client"
	if err := c.SaveGitOAuthApp(saved, 1); err != ErrGitOAuthConflict {
		t.Fatalf("changed identity: %v", err)
	}
	actual.Enabled = false
	if err := c.SaveGitOAuthApp(actual, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveGitOAuthApp(actual, 1); err != ErrGitOAuthConflict {
		t.Fatalf("stale revision: %v", err)
	}
	for _, bad := range []string{"http://public.example/api/git/oauth/callback", "https://agentbox.example/wrong", "https://agentbox.example/api/git/oauth/callback?secret=x"} {
		app.ID = "git-second"
		app.RedirectURL = bad
		if err := c.SaveGitOAuthApp(app, 0); err == nil {
			t.Fatalf("accepted redirect %s", bad)
		}
	}
}
