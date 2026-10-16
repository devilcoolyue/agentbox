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
	ext := c.GetExtensions()
	if ext.Gateway.Bind != defaultExtensionGatewayBind || ext.Gateway.Host != "172.17.0.1" {
		t.Fatalf("gateway defaults: %+v", ext.Gateway)
	}
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
	if got.Revision != 1 || string(got.Config) != `{"window":64}` || !got.Binds("other") || got.FailurePolicy != "fail_closed" {
		t.Fatalf("reloaded: %+v", got)
	}
	if err := reloaded.RemoveAccount("other"); err != nil {
		t.Fatal(err)
	}
	got = reloaded.GetExtensions().Items["agentbox.loop-guard"]
	if got.Binds("other") || !got.Binds("relay") || got.Revision != 2 {
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
	if _, err := Load(writeRaw(t, map[string]any{"auth_token": "test-token-long-enough", "extensions": map[string]any{"gateway": map[string]any{"bind": "0.0.0.0:1082"}}})); err == nil {
		t.Error("wildcard bind without host accepted")
	}
	if _, err := Load(writeRaw(t, map[string]any{"auth_token": "test-token-long-enough", "tunnel": map[string]any{"enabled": true},
		"extensions": map[string]any{"gateway": map[string]any{"bind": "172.17.0.1:1082"}}})); err == nil {
		t.Error("gateway bind colliding with the transparent network listener accepted")
	}
	if _, err := Load(writeRaw(t, map[string]any{"auth_token": "test-token-long-enough", "tunnel": map[string]any{"enabled": true}})); err != nil {
		t.Errorf("default gateway bind collides with tunnel defaults: %v", err)
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

func TestExtensionGatewayOptions(t *testing.T) {
	c := extensionsTestConfig(t, map[string]any{"extensions": map[string]any{"gateway": map[string]any{"bind": "172.17.0.1:1093"}}})
	gw, err := c.SetExtensionGatewayOptions(true, 0)
	if err != nil || !gw.DedicatedConnections || gw.Revision != 1 || gw.Bind != "172.17.0.1:1093" {
		t.Fatalf("set: %+v %v", gw, err)
	}
	if _, err := c.SetExtensionGatewayOptions(false, 0); !errors.Is(err, ErrExtensionRevision) {
		t.Fatalf("stale write: %v", err)
	}
	if err := c.ApplySettings(SettingsPatch{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.GetExtensions().Gateway; !got.DedicatedConnections || got.Revision != 1 || got.Bind != "172.17.0.1:1093" {
		t.Fatalf("reloaded: %+v", got)
	}
}
