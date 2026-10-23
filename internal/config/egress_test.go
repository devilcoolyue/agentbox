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
