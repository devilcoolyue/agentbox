package dockerx

import (
	"agentbox/internal/config"
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
