package config

import "testing"

func TestParsePortRange(t *testing.T) {
	for in, want := range map[string]PortRange{"30000-30999": {30000, 30999}, " 2000 - 2001 ": {2000, 2001}, "4000": {4000, 4000}} {
		got, err := ParsePortRange(in)
		if err != nil || got != want {
			t.Errorf("ParsePortRange(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "80-90", "3000-2000", "1-x", "60000-70000"} {
		if _, err := ParsePortRange(in); err == nil {
			t.Errorf("ParsePortRange(%q) accepted", in)
		}
	}
}

func TestDynamicPortRangeDefault(t *testing.T) {
	c := &Config{}
	if r := c.GetProxyBridge().DynamicPortRange(); r != (PortRange{30000, 30999}) {
		t.Fatalf("default range = %v", r)
	}
}

func TestValidateEgress(t *testing.T) {
	ok := []ContainerLimits{{Network: "bridge"}, {Network: "bridge", Egress: "open"}, {Network: "bridge", Egress: "restricted"}, {Network: "none", Egress: "restricted"}, {Network: "custom-net", Egress: "restricted"}}
	for _, l := range ok {
		if err := (&Config{Container: l}).validateEgress(); err != nil {
			t.Errorf("%+v rejected: %v", l, err)
		}
	}
	bad := []ContainerLimits{{Network: "bridge", Egress: "closed"}, {Network: "host", Egress: "restricted"}, {Network: "container:abc", Egress: "restricted"}}
	for _, l := range bad {
		if err := (&Config{Container: l}).validateEgress(); err == nil {
			t.Errorf("%+v accepted", l)
		}
	}
	if err := (&Config{Container: ContainerLimits{Network: "bridge"}, ProxyBridge: ProxyBridgeConfig{DynamicPorts: "80-90"}}).validateEgress(); err == nil {
		t.Error("privileged dynamic port range accepted")
	}
	if (ContainerLimits{Egress: "bogus"}).EgressMode() != EgressOpen {
		t.Error("unknown egress must normalize to open")
	}
}

func TestUnboundEgress(t *testing.T) {
	for _, v := range []string{"", "server", "none"} {
		if err := (&Config{Container: ContainerLimits{Network: "bridge", UnboundEgress: v}}).validateEgress(); err != nil {
			t.Errorf("%q rejected: %v", v, err)
		}
	}
	if err := (&Config{Container: ContainerLimits{Network: "bridge", UnboundEgress: "direct"}}).validateEgress(); err == nil {
		t.Error("unknown unbound_egress accepted")
	}
	for _, c := range []struct {
		l    ContainerLimits
		want bool
	}{
		{ContainerLimits{Egress: "restricted"}, true},
		{ContainerLimits{Egress: "restricted", UnboundEgress: "none"}, false},
		{ContainerLimits{Egress: "open"}, false},
	} {
		if got := c.l.UnboundServerDirect(); got != c.want {
			t.Errorf("%+v.UnboundServerDirect() = %v, want %v", c.l, got, c.want)
		}
	}
}

// A settings save from a client that does not know egress_exempt_users must
// keep the list; an explicit list replaces it (trimmed, de-duplicated).
func TestApplySettingsKeepsEgressExemptUsers(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	base := c.GetContainer()
	with := base
	with.EgressExemptUsers = []string{" boxadmin ", "boxadmin", "", "ops"}
	if err := c.ApplySettings(SettingsPatch{Container: &with}); err != nil {
		t.Fatal(err)
	}
	if got := c.GetContainer().EgressExemptUsers; len(got) != 2 || got[0] != "boxadmin" || got[1] != "ops" {
		t.Fatalf("exempt users = %q", got)
	}
	legacy := base
	legacy.MemoryMB = 4096
	legacy.EgressExemptUsers = nil
	if err := c.ApplySettings(SettingsPatch{Container: &legacy}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.GetContainer(); got.MemoryMB != 4096 || !got.EgressExempt("boxadmin") {
		t.Fatalf("legacy save dropped the list or the change: %+v", got)
	}
	cleared := base
	cleared.EgressExemptUsers = []string{}
	if err := c.ApplySettings(SettingsPatch{Container: &cleared}); err != nil {
		t.Fatal(err)
	}
	if c.GetContainer().EgressExempt("boxadmin") {
		t.Fatal("explicit empty list did not clear exemptions")
	}
}
