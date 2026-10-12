package agentprobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/collaboration"
)

var CollaborationRequired = []string{"claude_plan", "claude_result", "codex_plan", "codex_result"}

// CollaborationInput uses the same offline driver and actual CLI commands as
// the existing candidate gate. Only synthetic upstream response text differs.
func CollaborationInput() []byte {
	var spec map[string]any
	_ = json.Unmarshal(Input(), &spec)
	homes, err := collaborationProbeHomes()
	if err != nil {
		return nil
	}
	spec["home_files"] = homes
	spec["collaboration"] = true
	spec["handoffs"] = map[string]any{
		"plan":   map[string]any{"version": 1, "type": "plan", "payload": map[string]any{"summary": "Synthetic plan", "items": []any{map[string]any{"id": "fixture", "text": "Synthetic implementation", "acceptance": []string{"Synthetic assertion"}}}, "constraints": []string{}}},
		"result": map[string]any{"version": 1, "type": "result", "payload": map[string]any{"summary": "Synthetic implementation", "completed": []string{"fixture"}, "remaining": []string{}, "tests": []string{"Synthetic assertion"}}},
	}
	raw, _ := json.Marshal(spec)
	return raw
}

func (r Report) CollaborationPassed(imageID string) bool {
	if r.Version != 1 || imageID == "" || r.ImageID != imageID || r.CheckedAt <= 0 || r.Failure != "" || len(r.Checks) != len(CollaborationRequired) {
		return false
	}
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if !c.Passed || seen[c.Name] {
			return false
		}
		seen[c.Name] = true
	}
	for _, name := range CollaborationRequired {
		if !seen[name] {
			return false
		}
	}
	return true
}

// ValidateCollaboration passes real CLI output through the product transport,
// final-answer collector and handoff parser. Booleans from the driver alone
// never establish compatibility, and no production accounting is reachable.
func ValidateCollaboration(ctx context.Context, imageID string, raw []byte) Report {
	r := Report{Version: 1, ImageID: imageID, CheckedAt: time.Now().UnixMilli()}
	fail := func(code string) Report { r.Failure = code; return r }
	var output struct {
		Version int      `json:"version"`
		Checks  []string `json:"checks"`
		Records []record `json:"records"`
		Failed  string   `json:"failed"`
	}
	if ctx.Err() != nil {
		return fail("probe_cancelled")
	}
	if len(raw) > 4<<20 || json.Unmarshal(raw, &output) != nil || output.Version != 1 {
		return fail("collaboration_probe_invalid")
	}
	if output.Failed != "" {
		for _, name := range CollaborationRequired {
			if name == output.Failed {
				r.Stage = name
			}
		}
		return fail("collaboration_cli_failed")
	}
	if len(output.Records) != len(CollaborationRequired) {
		return fail("collaboration_probe_incomplete")
	}
	seen := map[string]bool{}
	for _, rec := range output.Records {
		r.Stage = rec.Name
		role := collaboration.Coordinator
		if strings.HasSuffix(rec.Name, "_result") {
			role = collaboration.Executor
		}
		if (rec.Name != rec.Agent+"_plan" && rec.Name != rec.Agent+"_result") || seen[rec.Name] || (rec.Agent != "claude" && rec.Agent != "codex") || rec.Requests < 1 || rec.Requests > 8 {
			return fail("collaboration_probe_invalid")
		}
		if role == collaboration.Executor && rec.Resume == "" {
			return fail("collaboration_resume_missing")
		}
		seen[rec.Name] = true
		final := agent.NewFinalOutput(rec.Agent, rec.Resume, collaboration.MaxHandoffBytes)
		if rec.Agent == "codex" {
			if len(rec.RPC) == 0 {
				return fail("collaboration_probe_invalid")
			}
			lines := make([]string, len(rec.RPC))
			for i, v := range rec.RPC {
				lines[i] = string(v)
			}
			if err := agent.RunCodexTurn(ctx, io.Discard, strings.NewReader(strings.Join(lines, "\n")+"\n"), func() {}, nil, agent.CodexTurn{Prompt: "synthetic", ThreadID: rec.Resume, StrictResume: true}, final.Observe); err != nil {
				return fail("collaboration_transport_failed")
			}
		} else {
			if rec.ExitCode == nil || *rec.ExitCode != 0 || rec.Signal != "" {
				return fail("collaboration_cli_failed")
			}
			for _, line := range rec.Lines {
				final.Observe(line)
			}
		}
		text, err := final.Result()
		if err != nil {
			return fail("collaboration_final_missing")
		}
		handoff, err := collaboration.ParseHandoff([]byte(text), role)
		if err != nil || (role == collaboration.Coordinator && handoff.Type != "plan") || (role == collaboration.Executor && handoff.Type != "result") {
			return fail("collaboration_handoff_invalid")
		}
		r.Checks = append(r.Checks, Check{Name: rec.Name, Passed: true})
	}
	r.Stage = ""
	if !r.CollaborationPassed(imageID) {
		return fail("collaboration_probe_incomplete")
	}
	return r
}

// Build the exact stripped participant configuration through the production
// seeder, with only synthetic provider settings. Temporary files never contain
// user login material and are not mounted into the candidate container.
func collaborationProbeHomes() (result map[string]map[string]string, resultErr error) {
	root, err := os.MkdirTemp("", "agentbox-collaboration-home-")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(root)) }()
	pool := filepath.Join(root, "pool")
	if err = os.Mkdir(pool, 0700); err != nil {
		return nil, err
	}
	provider := `model_provider="fixture"
[model_providers.fixture]
name="Synthetic"
base_url="AGENTBOX_PROBE_BASE/v1"
wire_api="responses"
env_key="OPENAI_API_KEY"
`
	if err = os.WriteFile(filepath.Join(pool, "config.toml"), []byte(provider), 0600); err != nil {
		return nil, err
	}
	homes := map[string]map[string]string{}
	for _, kind := range []string{"claude", "codex"} {
		home := filepath.Join(root, kind)
		if err = os.Mkdir(home, 0700); err != nil {
			return nil, err
		}
		model := "gpt-5.5"
		paths := []string{".codex/config.toml"}
		if kind == "claude" {
			model = "claude-sonnet-4-6"
			paths = []string{".claude.json", ".claude/settings.json"}
		}
		if err = agent.SeedCollaborationHome(kind, home, pool, model, os.Getuid(), os.Getgid()); err != nil {
			return nil, err
		}
		homes[kind] = map[string]string{}
		for _, path := range paths {
			raw, e := os.ReadFile(filepath.Join(home, path))
			if e != nil {
				return nil, e
			}
			homes[kind][path] = string(raw)
		}
	}
	return homes, nil
}
