package modelcatalog

import (
	"slices"
	"testing"

	"agentbox/internal/config"
)

func TestBuiltinSnapshotIsValid(t *testing.T) {
	if _, err := builtin(); err != nil {
		t.Fatal(err)
	}
	s := Builtin()
	for _, agent := range []string{config.AgentClaude, config.AgentCodex} {
		current := 0
		for _, m := range s.Models[agent] {
			if m.Reasoning == nil {
				t.Errorf("%s %s: the snapshot exists to supply reasoning", agent, m.ID)
			}
			if m.Current {
				current++
			}
		}
		if current == 0 || s.Sources[agent] == "" {
			t.Errorf("%s: no current models or source", agent)
		}
	}
	// Callers get a copy.
	s.Models[config.AgentClaude][0].Reasoning.Levels[0] = "changed"
	if Builtin().Models[config.AgentClaude][0].Reasoning.Levels[0] == "changed" {
		t.Fatal("Builtin shares state")
	}
}

func TestLookupIgnoresSnapshotDatesAndContextMarker(t *testing.T) {
	models := []Model{{ID: "claude-opus-4-5-20251101", Label: "Claude Opus 4.5"}, {ID: "claude-opus-5-5", Label: "Claude Opus 5.5"}}
	for _, id := range []string{"claude-opus-4-5", "claude-opus-4-5-20251101", "Claude-Opus-4-5"} {
		if m, ok := Lookup(models, id); !ok || m.Label != "Claude Opus 4.5" {
			t.Errorf("%s: %+v %v", id, m, ok)
		}
	}
	if m, ok := Lookup(models, "claude-opus-5-5[1m]"); !ok || m.ID != "claude-opus-5-5" {
		t.Errorf("[1m]: %+v %v", m, ok)
	}
	for _, id := range []string{"claude-opus-4-5-thinking", "claude-opus-5", "gpt-5.5"} {
		if _, ok := Lookup(models, id); ok {
			t.Errorf("%s: renamed or different models must not match", id)
		}
	}
}

func TestParseCLIMapsBothCatalogs(t *testing.T) {
	raw := []byte(`{"version":1,
	 "claude":{"models":[
	  {"value":"default","resolvedModel":"claude-opus-5-5[1m]","displayName":"Default (recommended)","description":"Use the default model","supportsEffort":true,"supportedEffortLevels":["low","high"]},
	  {"value":"opus[1m]","resolvedModel":"claude-opus-5-5[1m]","displayName":"Opus (1M context)","description":"Opus 5.5 with 1M context · Best for everyday tasks","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max","bogus"]},
	  {"value":"haiku","resolvedModel":"claude-haiku-4-5-20251001","displayName":"Haiku","description":"Haiku 4.5 · Fastest","supportsEffort":false,"supportedEffortLevels":[]},
	  {"value":"sonnet","resolvedModel":"sonnet","displayName":"Sonnet"},
	  {"value":"bad","resolvedModel":"claude-bad/id","displayName":"Bad"}]},
	 "codex":{"models":[
	  {"model":"gpt-6-sol","displayName":"GPT-6-Sol","hidden":false,"efforts":["low","medium","high","xhigh","max","ultra"]},
	  {"model":"codex-auto-review","displayName":"Codex Auto Review","hidden":true,"efforts":["low"]},
	  {"model":"gpt-plain","displayName":"","hidden":false,"efforts":[]}]}}`)
	models, failed, err := ParseCLI(raw)
	if err != nil || len(failed) != 0 {
		t.Fatal(err, failed)
	}
	claude := models[config.AgentClaude]
	if len(claude) != 2 || claude[0].ID != "claude-opus-5-5" || claude[0].Label != "Claude Opus 5.5" || !claude[0].Current {
		t.Fatalf("claude: %+v", claude)
	}
	if r := claude[0].Reasoning; r == nil || r.Control != "effort" || !slices.Equal(r.Levels, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("claude effort: %+v", r)
	}
	if claude[1].ID != "claude-haiku-4-5-20251001" || claude[1].Reasoning != nil || claude[1].Label != "Claude Haiku 4.5" {
		t.Fatalf("no effort report must stay unknown: %+v", claude[1])
	}
	codex := models[config.AgentCodex]
	if len(codex) != 2 || codex[0].ID != "gpt-6-sol" || len(codex[0].Reasoning.Levels) != 6 || codex[1].Label != "gpt-plain" || codex[1].Reasoning != nil {
		t.Fatalf("codex: %+v", codex)
	}

	models, failed, err = ParseCLI([]byte(`{"version":1,"claude":{"error":"timeout"},"codex":{"models":[{"model":"gpt-5.5","efforts":["low"]}]}}`))
	if err != nil || failed[config.AgentClaude] != "timeout" || len(models[config.AgentCodex]) != 1 || models[config.AgentClaude] != nil {
		t.Fatalf("partial: %+v %+v %v", models, failed, err)
	}
	for _, bad := range []string{``, `{}`, `{"version":2}`, `not json`} {
		if _, _, err := ParseCLI([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMergePrefersTheCLIAndKeepsOlderSnapshotModels(t *testing.T) {
	budget := &config.ReasoningCapability{Support: "supported", Control: "budget", Levels: []string{"low", "medium", "high", "xhigh"}}
	snapshot := []Model{
		{ID: "claude-opus-5-5", Label: "Claude Opus 5.5", Current: true, Reasoning: &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"low", "high"}}},
		{ID: "claude-haiku-4-5-20251001", Label: "Claude Haiku 4.5", Reasoning: budget},
		{ID: "claude-sonnet-5-5", Label: "Claude Sonnet 5.5", Current: true, Reasoning: budget},
	}
	cli := []Model{
		{ID: "claude-opus-5-5", Label: "Claude Opus 5.5 derived", Current: true, Reasoning: &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"low", "medium", "high", "xhigh", "max"}}},
		{ID: "claude-haiku-4-5-20251001", Label: "Claude Haiku 4.5", Current: true},
		{ID: "claude-next-6", Label: "Claude Next 6", Current: true},
	}
	out := Merge(cli, snapshot)
	ids := []string{}
	for _, m := range out {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"claude-opus-5-5", "claude-haiku-4-5-20251001", "claude-next-6", "claude-sonnet-5-5"}) {
		t.Fatal(ids)
	}
	if out[0].Label != "Claude Opus 5.5" || len(out[0].Reasoning.Levels) != 5 {
		t.Fatalf("CLI reasoning wins, snapshot label wins: %+v", out[0])
	}
	if out[1].Reasoning == nil || out[1].Reasoning.Control != "budget" {
		t.Fatalf("snapshot fills what the CLI does not report: %+v", out[1])
	}
	if out[2].Reasoning != nil || out[3].Current {
		t.Fatalf("unknown stays unknown; the CLI decides what is current: %+v", out)
	}
	out[1].Reasoning.Levels[0] = "changed"
	if budget.Levels[0] == "changed" {
		t.Fatal("Merge shares reasoning with its input")
	}
}
