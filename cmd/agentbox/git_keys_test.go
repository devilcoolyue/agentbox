package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
)

func TestGitKeyRotateOfflineResumeAndMissingKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(map[string]any{"auth_token": "fixture-auth", "data_dir": dir, "accounts": []any{}})
	if err := os.WriteFile(cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	v := &gitaccess.Vault{DataDir: dir}
	c := store.GitConnection{ID: "fixture", Owner: "alice", Label: "fixture", Provider: "gitlab", BaseURL: "https://git.example.com", AuthType: "pat", Enabled: true}
	c.Secret, err = v.Seal([]byte("fixture-pat"), c.AssociatedData(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.SaveGitConnection(c, 0); err != nil {
		t.Fatal(err)
	}
	app := config.GitOAuthApp{ID: "fixture-app", Label: "fixture", Provider: "gitlab", BaseURL: c.BaseURL, ClientID: "fixture-client", RedirectURL: "https://box.example/api/git/oauth/callback", Enabled: true}
	app.Secret, err = v.Seal([]byte("fixture-app-secret"), app.AssociatedData(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.SaveGitOAuthApp(app, 0); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", cfgPath}
	lock, err := os.OpenFile(filepath.Join(dir, "agentbox.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = rotateGitKeys(t.Context(), args); err == nil {
		t.Fatal("rotated running instance")
	}
	lock.Close()
	if err = rotateGitKeys(t.Context(), append(args, "--resume")); err == nil {
		t.Fatal("resumed legacy-only instance")
	}
	// Simulate interruption immediately after activating a key.
	id, err := v.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if err = rotateGitKeys(t.Context(), append(args, "--resume")); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		saved, err := st.GitConnection("alice", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := v.Open(saved.Secret, saved.AssociatedData())
		if err != nil || string(plain) != "fixture-pat" || saved.Revision != 1 || string(saved.Secret[1:33]) != want {
			t.Fatal("connection rekey failed", err)
		}
		loaded, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := loaded.GitOAuthApp(app.ID)
		plain, err = v.Open(a.Secret, a.AssociatedData())
		if err != nil || string(plain) != "fixture-app-secret" || a.Revision != 1 || string(a.Secret[1:33]) != want {
			t.Fatal("app rekey failed", err)
		}
	}
	check(id)
	if err = rotateGitKeys(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	next, err := v.ActiveKeyID()
	if err != nil || id == next {
		t.Fatal("no new key")
	}
	check(next)
	// Failed transform must roll back every previously rewritten row.
	before, _ := st.GitConnection("alice", c.ID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = rotateGitKeys(ctx, args); err == nil {
		t.Fatal("cancel ignored")
	}
	after, _ := st.GitConnection("alice", c.ID)
	if !bytes.Equal(before.Secret, after.Secret) {
		t.Fatal("cancel mutated credentials")
	}
	if err = os.Remove(filepath.Join(dir, "git-secrets/keyring.json")); err != nil {
		t.Fatal(err)
	}
	if err = rotateGitKeys(t.Context(), args); err == nil {
		t.Fatal("missing key silently replaced")
	}
	if _, err = os.Stat(filepath.Join(dir, "git-secrets/keyring.json")); !os.IsNotExist(err) {
		t.Fatal("replacement key written")
	}
}
