package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Unlike CODEX_LIVE_TEST this uses no real credentials or inference: the
// container cannot access the network and all provider responses are synthetic.
func TestReasoningCLIWire(t *testing.T) {
	image := os.Getenv("AGENTBOX_CLI_TEST_IMAGE")
	if image == "" {
		t.Skip("set AGENTBOX_CLI_TEST_IMAGE to test a local Linux agent image")
	}
	script, err := os.ReadFile("testdata/reasoning-probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	command := func(kind, model, effort, control string) []string {
		cmd, err := ChatCommand(kind, "bypassPermissions", "", model, effort, control)
		if err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	cases := []map[string]any{
		{"name": "claude-native", "command": command("claude", "claude-sonnet-4-6", "low", "effort"), "env": map[string]string{"MAX_THINKING_TOKENS": "13000", "CLAUDE_CODE_EFFORT_LEVEL": "high", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING": "1"}},
		{"name": "claude-budget", "command": command("claude", "claude-sonnet-4-5", "medium", "budget")},
		{"name": "claude-default", "command": command("claude", "claude-sonnet-4-6", "", ""), "claudeSettings": map[string]any{"effortLevel": "low"}},
		{"name": "codex-exec", "command": command("codex", "gpt-5.5", "low", "effort"), "codexConfig": "model_reasoning_effort=\"high\""},
		{"name": "codex-appserver", "protocol": true, "model": "gpt-5.5", "effort": "low", "summary": CodexReasoningSummary, "codexConfig": "model_reasoning_effort=\"high\""},
		{"name": "codex-default", "protocol": true, "model": "gpt-5.5", "codexConfig": "model_reasoning_effort=\"high\""},
	}
	raw, _ := json.Marshal(cases)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "--user", "1000:1000", "--entrypoint", "node", image, "-e", string(script))
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI fixture: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(cases) {
		t.Fatalf("missing fixture output: %s", out)
	}
	for _, line := range lines {
		var result struct {
			Name     string `json:"name"`
			Requests []struct {
				Model    string `json:"model"`
				Thinking struct {
					Type    string `json:"type"`
					Budget  int    `json:"budget_tokens"`
					Display string `json:"display"`
				} `json:"thinking"`
				Output struct {
					Effort string `json:"effort"`
				} `json:"output_config"`
				Reasoning struct {
					Effort  string `json:"effort"`
					Summary string `json:"summary"`
				} `json:"reasoning"`
			} `json:"requests"`
		}
		if err := json.Unmarshal([]byte(line), &result); err != nil || len(result.Requests) == 0 {
			t.Errorf("no captured request: %s", line)
			continue
		}
		r := result.Requests[0]
		switch result.Name {
		case "claude-native", "claude-default":
			if r.Output.Effort != "low" || r.Thinking.Budget != 0 || r.Thinking.Display != "summarized" {
				t.Errorf("%s", line)
			}
		case "claude-budget":
			if r.Thinking.Budget != 13000 || r.Output.Effort != "" || r.Thinking.Display != "summarized" {
				t.Errorf("%s", line)
			}
		case "codex-exec", "codex-appserver":
			if r.Reasoning.Effort != "low" || r.Reasoning.Summary != CodexReasoningSummary {
				t.Errorf("%s", line)
			}
		case "codex-default":
			if r.Reasoning.Effort != "high" {
				t.Errorf("%s", line)
			}
		}
		t.Log(line)
	}
}

func TestReasoningCLICatalog(t *testing.T) {
	image := os.Getenv("AGENTBOX_CLI_TEST_IMAGE")
	if image == "" {
		t.Skip("requires local Linux image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "--user", "1000:1000", "--entrypoint", "codex", image, "app-server")
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	models, probeErr := ProbeCodexModels(w, r)
	w.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if probeErr != nil || len(models) == 0 {
		t.Fatalf("catalog: %v (%d models)", probeErr, len(models))
	}
	t.Logf("read %d model capability entries without a thread or provider request", len(models))
}
