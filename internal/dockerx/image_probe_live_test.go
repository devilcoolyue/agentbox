package dockerx

import (
	"agentbox/internal/config"
	"agentbox/internal/modelcatalog"
	"os"
	"testing"
)

func TestCLIImageProbeLive(t *testing.T) {
	ref := os.Getenv("AGENTBOX_CLI_PROBE_TEST_IMAGE")
	if ref == "" {
		t.Skip("requires an explicitly selected local Agent image; synthetic upstream only")
	}
	m, err := New(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	candidate, err := m.InspectCLIImage(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	report, err := m.ValidateCLIImage(t.Context(), candidate.ID)
	if err != nil || !report.Passed(candidate.ID) {
		t.Fatalf("candidate report: %+v; %v", report, err)
	}
	t.Logf("candidate %s: %+v", candidate.ID, report)
}

// Both CLIs must answer signed out with network=none; no provider is called.
func TestCLICatalogLive(t *testing.T) {
	ref := os.Getenv("AGENTBOX_CLI_PROBE_TEST_IMAGE")
	if ref == "" {
		t.Skip("requires an explicitly selected local Agent image; offline sandbox only")
	}
	m, err := New(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	img, err := m.InspectCLIImage(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := m.ReadCLICatalog(t.Context(), img.ID)
	if err != nil {
		t.Fatal(err)
	}
	models, failed, err := modelcatalog.ParseCLI(raw)
	if err != nil || len(failed) != 0 || len(models[config.AgentClaude]) == 0 || len(models[config.AgentCodex]) == 0 {
		t.Fatalf("catalog: %+v failed=%v err=%v", models, failed, err)
	}
	t.Logf("claude %s: %+v", img.Claude, models[config.AgentClaude])
	t.Logf("codex %s: %+v", img.Codex, models[config.AgentCodex])
}
