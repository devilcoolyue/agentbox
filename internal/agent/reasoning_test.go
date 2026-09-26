package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
)

func TestReasoningValidation(t *testing.T) {
	known := &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"low", "high"}}
	for _, tc := range []struct {
		kind, level, control string
		policy               *config.ReasoningCapability
		wantErr              bool
	}{
		{"codex", "high", "effort", known, false},
		{"codex", "xhigh", "effort", known, true},
		{"codex", "", "", known, false},
		{"codex", "high", "", &config.ReasoningCapability{Support: "unsupported"}, true},
		{"codex", "xhigh", "", nil, false},
		{"codex", "invalid", "", nil, true},
		{"claude", "high", "", known, true}, // stale pre-upgrade budget
		{"claude", "high", "effort", known, false},
		{"claude", "xhigh", "budget", nil, false},
	} {
		_, err := ResolveTurnOptions(tc.kind, "fixture", tc.level, tc.control, tc.policy)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestClaudeEffortAndBudgetAreDistinct(t *testing.T) {
	for _, mode := range []string{"effort", "budget"} {
		cmd, err := ChatCommand("claude", "bypassPermissions", "", "fixture", "high", mode)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "--effort high") != (mode == "effort") {
			t.Fatal(joined)
		}
		if strings.Contains(joined, "MAX_THINKING_TOKENS=24000") != (mode == "budget") {
			t.Fatal(joined)
		}
	}
}

func TestUnsupportedModelRejectsInheritedConfiguration(t *testing.T) {
	for _, tc := range []struct{ kind, file, body string }{
		{"codex", ".codex/config.toml", "model_reasoning_effort = 'high'"},
		{"codex", ".codex/config.toml", "profile = 'relay'\n[profiles.relay]\nmodel_reasoning_effort = 'xhigh'"},
		{"claude", ".claude/settings.json", `{"effortLevel":"high"}`},
		{"claude", ".claude/settings.json", `{"env":{"MAX_THINKING_TOKENS":"4096"}}`},
	} {
		dir := t.TempDir()
		os.MkdirAll(filepath.Dir(filepath.Join(dir, tc.file)), 0700)
		os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.body), 0600)
		options := TurnOptions{Model: "fixture", Unsupported: true}
		if err := CheckReasoningInheritance(tc.kind, dir, t.TempDir(), nil, options); err == nil {
			t.Fatalf("accepted %s", tc.body)
		}
		options.Unsupported = false
		if err := CheckReasoningInheritance(tc.kind, dir, t.TempDir(), nil, options); err != nil {
			t.Fatalf("inherit default must remain valid: %v", err)
		}
	}
}

func TestTurnStartFailuresNeverFallback(t *testing.T) {
	for _, last := range []string{`{"id":4,"error":{"code":-32602,"message":"unsupported effort"}}`, ""} {
		script := "{\"id\":1,\"result\":{}}\n{\"id\":3,\"result\":{\"thread\":{\"id\":\"T1\"}}}\n" + last + "\n"
		err := RunCodexTurn(t.Context(), &bytes.Buffer{}, strings.NewReader(script), func() {}, nil, CodexTurn{Prompt: "fixture"}, func([]byte) {})
		if err == nil || errors.Is(err, ErrAppServerUnavailable) {
			t.Fatalf("unsafe fallback: %v", err)
		}
	}
}

func TestUnsupportedModelRejectsResumedEffortBeforeSubmission(t *testing.T) {
	script := "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{\"thread\":{\"id\":\"T1\"},\"reasoningEffort\":\"high\"}}\n"
	var sent bytes.Buffer
	err := RunCodexTurn(context.Background(), &sent, strings.NewReader(script), func() {}, nil, CodexTurn{ThreadID: "T1", Model: "fixture", RejectInheritedEffort: true}, func([]byte) {})
	if err == nil || errors.Is(err, ErrAppServerUnavailable) || strings.Contains(sent.String(), "turn/start") {
		t.Fatalf("submitted unsupported inherited effort: %v %s", err, sent.String())
	}
}
