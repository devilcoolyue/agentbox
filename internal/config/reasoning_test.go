package config

import (
	"os"
	"reflect"
	"testing"
)

func TestReasoningPoliciesPersistCloneAndOverride(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	models := c.GetModels()
	model := models[AgentCodex][0].ID
	models[AgentCodex][0].Reasoning = &ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"low", "high"}}
	if err := c.ApplySettings(SettingsPatch{Models: models}); err != nil {
		t.Fatal(err)
	}
	models[AgentCodex][0].Reasoning.Levels[0] = "corrupted"
	if c.GetModels()[AgentCodex][0].Reasoning.Levels[0] != "low" {
		t.Fatal("patch aliases persisted policy")
	}
	got := c.GetModels()
	got[AgentCodex][0].Reasoning.Levels[0] = "corrupted"
	a := Account{ID: "reasoning", Type: AgentCodex, ModelReasoning: map[string]ReasoningCapability{model: {Support: "unsupported"}}}
	if err := c.AddAccount(a); err != nil {
		t.Fatal(err)
	}
	a.ModelReasoning[model] = ReasoningCapability{Support: "unknown"}
	saved, _ := c.Account(a.ID)
	if c.ConfiguredReasoning(saved, model).Support != "unsupported" {
		t.Fatal("account must override model policy")
	}
	if c.ConfiguredReasoning(Account{Type: AgentCodex}, model).Levels[0] != "low" {
		t.Fatal("model getter aliases policy")
	}
	label := "renamed"
	if _, err := c.UpdateAccount(a.ID, AccountPatch{Label: &label}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	reloadedAcct, _ := reloaded.Account(a.ID)
	if !reflect.DeepEqual(reloadedAcct.ModelReasoning, saved.ModelReasoning) {
		t.Fatal("unrelated mutation lost account policy")
	}
	if reloaded.GetModels()[AgentCodex][0].Reasoning.Levels[0] != "low" {
		t.Fatal("unrelated mutation lost model policy")
	}
	empty := map[string]ReasoningCapability{}
	updated, err := c.UpdateAccount(a.ID, AccountPatch{ModelReasoning: &empty})
	if err != nil || c.ConfiguredReasoning(updated, model).Support != "supported" {
		t.Fatalf("clear override: %v", err)
	}
}

func TestReasoningRejectsInvalidPolicyAtomically(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	before, _ := os.ReadFile(c.path)
	for _, r := range []ReasoningCapability{
		{Support: "typo"},
		{Support: "supported", Control: "effort"},
		{Support: "supported", Control: "budget", Levels: []string{"high"}},
		{Support: "unsupported", Control: "effort"},
		{Support: "supported", Control: "effort", Levels: []string{"invalid"}},
		{Support: "supported", Control: "effort", Levels: []string{"low", "low"}},
	} {
		models := c.GetModels()
		models[AgentCodex][0].Reasoning = &r
		if err := c.ApplySettings(SettingsPatch{Models: models}); err == nil {
			t.Fatalf("accepted %+v", r)
		}
		if c.GetModels()[AgentCodex][0].Reasoning != nil {
			t.Fatal("failed patch mutated memory")
		}
		after, _ := os.ReadFile(c.path)
		if string(before) != string(after) {
			t.Fatal("failed patch changed disk")
		}
	}
}
