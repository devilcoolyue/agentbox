package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func extensionsTestConfig(t *testing.T, extra map[string]any) *Config {
	t.Helper()
	raw := map[string]any{"auth_token": "test-token-long-enough", "data_dir": t.TempDir(), "accounts": []map[string]any{
		{"id": "relay", "type": "claude"}, {"id": "other", "type": "claude"},
	}}
	for k, v := range extra {
		raw[k] = v
	}
	b, _ := json.Marshal(raw)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExtensionSettingPersistenceAndRevision(t *testing.T) {
	c := extensionsTestConfig(t, nil)
	s := ExtensionSetting{Enabled: true, Config: json.RawMessage(`{"window":64}`), Permissions: []string{"model.stream.text"},
		Bindings: ExtensionBindings{Mode: "accounts", Accounts: []string{"relay", "other"}}, FailurePolicy: "fail_closed"}
	saved, err := c.SetExtension("agentbox.loop-guard", s, 0)
	if err != nil || saved.Revision != 1 {
		t.Fatalf("save: %+v %v", saved, err)
	}
	if _, err := c.SetExtension("agentbox.loop-guard", s, 0); !errors.Is(err, ErrExtensionRevision) {
		t.Fatalf("stale write: %v", err)
	}
	// Other config writes keep the section (mutate copies every field).
	if err := c.ApplySettings(SettingsPatch{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.GetExtensions().Items["agentbox.loop-guard"]
	if got.Revision != 1 || string(got.Config) != `{"window":64}` || !got.Binds(BindingTarget{Account: "other"}) || got.FailurePolicy != "fail_closed" {
		t.Fatalf("reloaded: %+v", got)
	}
	if err := reloaded.RemoveAccount("other"); err != nil {
		t.Fatal(err)
	}
	got = reloaded.GetExtensions().Items["agentbox.loop-guard"]
	if got.Binds(BindingTarget{Account: "other"}) || !got.Binds(BindingTarget{Account: "relay"}) || got.Revision != 2 || got.Bindings.Mode != BindingModeSelected {
		t.Fatalf("prune: %+v", got)
	}
}

func TestExtensionValidation(t *testing.T) {
	c := extensionsTestConfig(t, nil)
	for name, s := range map[string]ExtensionSetting{
		"unknown account": {Bindings: ExtensionBindings{Mode: "accounts", Accounts: []string{"ghost"}}},
		"duplicate":       {Bindings: ExtensionBindings{Mode: "accounts", Accounts: []string{"relay", "relay"}}},
		"bad mode":        {Bindings: ExtensionBindings{Mode: "some"}},
		"accounts in all": {Bindings: ExtensionBindings{Mode: "all", Accounts: []string{"relay"}}},
		"users in none":   {Bindings: ExtensionBindings{Mode: "none", Users: []string{"alice"}}},
		"duplicate user":  {Bindings: ExtensionBindings{Mode: "selected", Users: []string{"alice", "alice"}}},
		"bad policy":      {Bindings: ExtensionBindings{Mode: "none"}, FailurePolicy: "maybe"},
		"bad config":      {Bindings: ExtensionBindings{Mode: "none"}, Config: json.RawMessage(`{`)},
	} {
		if _, err := c.SetExtension("agentbox.loop-guard", s, 0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := c.SetExtension("Bad ID", ExtensionSetting{Bindings: ExtensionBindings{Mode: "none"}}, 0); err == nil {
		t.Error("bad id accepted")
	}
}

func writeRaw(t *testing.T, raw map[string]any) string {
	t.Helper()
	raw["data_dir"] = t.TempDir()
	b, _ := json.Marshal(raw)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Bindings select accounts and users; the pre-2026-10-17 accounts mode is
// read as selected, and deleted users are pruned like deleted accounts.
func TestExtensionBindingsSelectUsersAndAccounts(t *testing.T) {
	c := extensionsTestConfig(t, map[string]any{"extensions": map[string]any{"items": map[string]any{
		"agentbox.legacy": map[string]any{"enabled": true, "bindings": map[string]any{"mode": "accounts", "accounts": []string{"relay"}}},
	}}})
	legacy := c.GetExtensions().Items["agentbox.legacy"]
	if legacy.Bindings.Mode != BindingModeSelected || !legacy.Binds(BindingTarget{Account: "relay", User: "bob"}) {
		t.Fatalf("legacy mode: %+v", legacy.Bindings)
	}
	s := ExtensionSetting{Enabled: true, Bindings: ExtensionBindings{Mode: "selected", Accounts: []string{"relay"}, Users: []string{"alice", "bob"}}}
	if _, err := c.SetExtension("agentbox.webhook", s, 0); err != nil {
		t.Fatal(err)
	}
	got := c.GetExtensions().Items["agentbox.webhook"]
	for target, want := range map[BindingTarget]bool{
		{User: "alice"}:                   true,
		{Account: "relay"}:                true,
		{Account: "other", User: "carol"}: false,
		{}:                                false,
	} {
		if got.Binds(target) != want {
			t.Errorf("Binds(%+v) = %v", target, !want)
		}
	}
	if (ExtensionSetting{Bindings: ExtensionBindings{Mode: "all"}}).Binds(BindingTarget{}) != true {
		t.Error("mode all")
	}
	if err := c.PruneExtensionUser("alice"); err != nil {
		t.Fatal(err)
	}
	got = c.GetExtensions().Items["agentbox.webhook"]
	if got.Binds(BindingTarget{User: "alice"}) || !got.Binds(BindingTarget{User: "bob"}) || got.Revision != 2 {
		t.Fatalf("prune user: %+v", got)
	}
	if c.GetExtensions().Items["agentbox.legacy"].Revision != 0 {
		t.Fatal("unrelated extension touched by the prune")
	}
}
