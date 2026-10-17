package config

import (
	"os"
	"strings"
	"testing"
)

func TestModelGatewayDefaultsAndSettings(t *testing.T) {
	c := extensionsTestConfig(t, nil)
	if gw := c.GetModelGateway(); gw.Bind != defaultModelGatewayBind || gw.Host != "172.17.0.1" || gw.DedicatedConnections {
		t.Fatalf("defaults: %+v", gw)
	}
	on := true
	if err := c.ApplySettings(SettingsPatch{ModelGateway: &ModelGatewayPatch{DedicatedConnections: &on}}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if gw := reloaded.GetModelGateway(); !gw.DedicatedConnections || gw.Bind != defaultModelGatewayBind {
		t.Fatalf("reloaded: %+v", gw)
	}
}

// Configs written before the gateway left the extensions section keep their
// listener and connection mode, and are rewritten in the new shape.
func TestModelGatewayMigratesLegacySection(t *testing.T) {
	c := extensionsTestConfig(t, map[string]any{"extensions": map[string]any{
		"gateway": map[string]any{"bind": "172.17.0.1:1093", "dedicated_connections": true, "revision": 4},
		"items":   map[string]any{"agentbox.loop-guard": map[string]any{"enabled": true, "bindings": map[string]any{"mode": "all"}}},
	}})
	if gw := c.GetModelGateway(); gw.Bind != "172.17.0.1:1093" || !gw.DedicatedConnections {
		t.Fatalf("migrated: %+v", gw)
	}
	if !c.GetExtensions().Items["agentbox.loop-guard"].Enabled {
		t.Fatal("extension items lost in migration")
	}
	if err := c.ApplySettings(SettingsPatch{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"model_gateway"`) || strings.Contains(string(raw), `"gateway": {`) {
		t.Fatalf("saved file still uses the legacy section:\n%s", raw)
	}
	// An explicit model_gateway wins over a leftover legacy section.
	c = extensionsTestConfig(t, map[string]any{"model_gateway": map[string]any{"bind": "172.17.0.1:1094"},
		"extensions": map[string]any{"gateway": map[string]any{"bind": "172.17.0.1:1093"}}})
	if gw := c.GetModelGateway(); gw.Bind != "172.17.0.1:1094" {
		t.Fatalf("explicit section: %+v", gw)
	}
}

func TestModelGatewayValidation(t *testing.T) {
	if _, err := Load(writeRaw(t, map[string]any{"auth_token": "test-token-long-enough", "model_gateway": map[string]any{"bind": "0.0.0.0:1082"}})); err == nil {
		t.Error("wildcard bind without host accepted")
	}
	if _, err := Load(writeRaw(t, map[string]any{"auth_token": "test-token-long-enough", "tunnel": map[string]any{"enabled": true},
		"model_gateway": map[string]any{"bind": "172.17.0.1:1082"}})); err == nil {
		t.Error("gateway bind colliding with the transparent network listener accepted")
	}
	if _, err := Load(writeRaw(t, map[string]any{"auth_token": "test-token-long-enough", "tunnel": map[string]any{"enabled": true}})); err != nil {
		t.Errorf("default gateway bind collides with tunnel defaults: %v", err)
	}
	// A legacy section is validated after migration, under its new name.
	if _, err := Load(writeRaw(t, map[string]any{"auth_token": "test-token-long-enough", "extensions": map[string]any{"gateway": map[string]any{"bind": "nope"}}})); err == nil || !strings.Contains(err.Error(), "model_gateway.bind") {
		t.Errorf("legacy invalid bind: %v", err)
	}
}
