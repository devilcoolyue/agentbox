package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountModelListNormalizesAndValidates(t *testing.T) {
	a := Account{ID: "relay", Type: AgentCodex, Models: []ModelOption{{ID: " relay-a ", Label: ""}, {ID: "relay-b", Label: "B"}}}
	if err := a.normalizeModels(); err != nil {
		t.Fatal(err)
	}
	if a.Models[0].ID != "relay-a" || a.Models[0].Label != "relay-a" || a.DefaultModel != "relay-a" {
		t.Fatalf("not normalized: %+v", a)
	}
	if !a.RestrictsModels() || !a.AllowsModel("relay-b") || a.AllowsModel("gpt-5.5") {
		t.Fatal("list does not restrict")
	}
	if a.ResolveModel("relay-b") != "relay-b" || a.ResolveModel("gpt-5.5") != "relay-a" {
		t.Fatal("resolve did not fall back to the account default")
	}
	if open := (Account{Type: AgentCodex}); !open.AllowsModel("anything") || open.ResolveModel("anything") != "anything" {
		t.Fatal("an account without a list must keep the global behavior")
	}
	for name, bad := range map[string]Account{
		"unknown default": {Type: AgentCodex, Models: []ModelOption{{ID: "relay-a"}}, DefaultModel: "relay-b"},
		"default no list": {Type: AgentCodex, DefaultModel: "relay-a"},
		"duplicate":       {Type: AgentCodex, Models: []ModelOption{{ID: "relay-a"}, {ID: "relay-a"}}},
		"invalid id":      {Type: AgentCodex, Models: []ModelOption{{ID: "org/model"}}},
		"budget on codex": {Type: AgentCodex, Models: []ModelOption{{ID: "relay-a", Reasoning: &ReasoningCapability{Support: "supported", Control: "budget", Levels: []string{"low"}}}}},
		"long label":      {Type: AgentCodex, Models: []ModelOption{{ID: "relay-a", Label: strings.Repeat("长", 81)}}},
		"hidden default":  {Type: AgentCodex, Models: []ModelOption{{ID: "relay-a", Hidden: true}, {ID: "relay-b"}}, DefaultModel: "relay-a"},
		"all hidden":      {Type: AgentCodex, Models: []ModelOption{{ID: "relay-a", Hidden: true}}},
	} {
		if err := bad.normalizeModels(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestHiddenAccountModelsStayAllowed(t *testing.T) {
	a := Account{Type: AgentCodex, Models: []ModelOption{{ID: "old", Hidden: true}, {ID: "new"}}}
	if err := a.normalizeModels(); err != nil {
		t.Fatal(err)
	}
	// The default skips hidden models; a hidden model is still accepted.
	if a.DefaultModel != "new" || a.ShowsModel("old") || !a.ShowsModel("new") || !a.AllowsModel("old") || a.ResolveModel("old") != "old" {
		t.Fatalf("hidden handling: %+v", a)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"data_dir": t.TempDir(), "auth_token": "test-token-long-enough", "agent_image": "test",
		"models": map[string][]ModelOption{AgentCodex: {{ID: "gpt-5.5", Label: "GPT-5.5", Hidden: true}}}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("hidden accepted on the global list: %v", err)
	}
}

func TestAccountModelListPersistsAndOutranksGlobalCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"data_dir": t.TempDir(), "auth_token": "test-token-long-enough", "agent_image": "test",
		"accounts": []Account{{ID: "relay", Type: AgentCodex}},
		"models":   map[string][]ModelOption{AgentCodex: {{ID: "gpt-5.5", Label: "GPT-5.5", Reasoning: &ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"low", "high"}}}}},
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	models := []ModelOption{{ID: "gpt-5.5", Label: "GPT-5.5 (relay)", Reasoning: &ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"medium"}}}, {ID: "relay-plain"}}
	def := "relay-plain"
	if _, err := cfg.UpdateAccount("relay", AccountPatch{Models: &models, DefaultModel: &def}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := reloaded.Account("relay")
	if len(acct.Models) != 2 || acct.DefaultModel != "relay-plain" || acct.Models[0].Label != "GPT-5.5 (relay)" {
		t.Fatalf("model list not persisted: %+v", acct)
	}
	if r := reloaded.ConfiguredReasoning(acct, "gpt-5.5"); r == nil || strings.Join(r.Levels, ",") != "medium" {
		t.Fatalf("account capability did not outrank the global list: %+v", r)
	}
	if r := reloaded.ConfiguredReasoning(acct, "relay-plain"); r != nil {
		t.Fatalf("unknown model got a capability: %+v", r)
	}
	if reloaded.NewWorkspaceModel(acct) != "relay-plain" || reloaded.NewWorkspaceModel(Account{Type: AgentCodex}) != "gpt-5.5" {
		t.Fatal("new workspace model ignores the account list")
	}
	// An empty list returns the account to the global list and clears the default.
	empty, none := []ModelOption{}, ""
	if _, err := reloaded.UpdateAccount("relay", AccountPatch{Models: &empty, DefaultModel: &none}); err != nil {
		t.Fatal(err)
	}
	if acct, _ := reloaded.Account("relay"); acct.RestrictsModels() || acct.DefaultModel != "" {
		t.Fatalf("list not cleared: %+v", acct)
	}
	bad := "missing"
	if _, err := reloaded.UpdateAccount("relay", AccountPatch{Models: &models, DefaultModel: &bad}); err == nil {
		t.Fatal("default outside the list accepted")
	}
}
