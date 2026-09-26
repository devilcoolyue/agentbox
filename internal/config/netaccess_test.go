package config

import "testing"

func TestTransparentSettingsPersistAcrossMutations(t *testing.T) {
	cfg := writeConfig(t, minimalConfig)
	patch := TunnelConfig{Enabled: true, Transparent: true, NetworkImage: "fixture-network:v1", NetworkBind: "172.17.0.1:1082"}
	if err := cfg.ApplySettings(SettingsPatch{Tunnel: &patch}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddAccount(Account{ID: "network-test", Type: AgentCodex}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(cfg.path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.GetTunnel()
	if !got.Transparent || got.NetworkImage != patch.NetworkImage || got.NetworkBind != patch.NetworkBind || got.ProxyHost == "" {
		t.Fatal(got)
	}
}
func TestTransparentRejectsSharedNetwork(t *testing.T) {
	for _, network := range []string{"host", "none", "container:other"} {
		cfg := writeConfig(t, minimalConfig)
		before := cfg.GetTunnel()
		limits := cfg.GetContainer()
		limits.Network = network
		tc := TunnelConfig{Enabled: true, Transparent: true}
		if err := cfg.ApplySettings(SettingsPatch{Tunnel: &tc, Container: &limits}); err == nil {
			t.Fatalf("accepted %s", network)
		}
		if cfg.GetTunnel() != before {
			t.Fatal("failed mutation changed config")
		}
	}
}

func TestTransparentDefaultAndExplicitCompatibility(t *testing.T) {
	cfg := writeConfig(t, minimalConfig)
	if !cfg.GetTunnel().Transparent || cfg.GetTunnel().Enabled {
		t.Fatal("default mode must not enable tunnel")
	}
	legacy := TunnelConfig{Enabled: true, Transparent: false}
	if err := cfg.ApplySettings(SettingsPatch{Tunnel: &legacy}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(cfg.path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GetTunnel().Transparent {
		t.Fatal("explicit compatibility mode lost during reload")
	}
	if err := loaded.AddAccount(Account{ID: "mode-test", Type: AgentCodex}); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(cfg.path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GetTunnel().Transparent {
		t.Fatal("unrelated config mutation lost compatibility mode")
	}
}
