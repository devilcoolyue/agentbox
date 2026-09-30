package config

import "testing"

func TestImageUpdatesPersistenceAndCAS(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	p, base, old := c.ImageUpdateState()
	if p.Enabled || p.Channel != "stable" || p.Time != "04:00" || old != "" {
		t.Fatalf("defaults: %+v", p)
	}
	p = ImageUpdateConfig{Enabled: true, Channel: "latest", Time: "23:59", UpdateCodex: true}
	if err := c.ApplySettings(SettingsPatch{ImageUpdates: &p}); err != nil {
		t.Fatal(err)
	}
	if err := c.SwitchAgentImage(base, p, "candidate", "old-id", false); err != nil {
		t.Fatal(err)
	}
	idle := int64(9)
	if err := c.ApplySettings(SettingsPatch{IdleTimeoutMin: &idle}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	actual, image, previous := reloaded.ImageUpdateState()
	if actual != p || image != "candidate" || previous != "old-id" {
		t.Fatalf("lost persisted state: %+v %s %s", actual, image, previous)
	}
	if err := c.SwitchAgentImage(base, p, "stale", "stale", false); err == nil {
		t.Fatal("stale image accepted")
	}
	disabled := p
	disabled.Enabled = false
	if err := c.ApplySettings(SettingsPatch{ImageUpdates: &disabled}); err != nil {
		t.Fatal(err)
	}
	if err := c.SwitchAgentImage("candidate", p, "stale", "stale", false); err == nil {
		t.Fatal("stale policy accepted")
	}
	if err := c.SwitchAgentImage("candidate", disabled, "old-id", "new-id", true); err != nil {
		t.Fatal(err)
	}
	final, _, _ := c.ImageUpdateState()
	if final.Enabled {
		t.Fatal("rollback left automatic updates enabled")
	}
	next := "manual"
	if err := c.ApplySettings(SettingsPatch{AgentImage: &next, ExpectedAgentImage: &base}); err == nil {
		t.Fatal("stale settings form overwrote active image")
	}
}
func TestImageUpdateValidation(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	for _, p := range []ImageUpdateConfig{{Channel: "next"}, {Time: "24:00"}, {Time: "4:00"}, {Time: "12:60"}, {Time: "\n04:00"}} {
		if err := c.ApplySettings(SettingsPatch{ImageUpdates: &p}); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}
