// Package agentprobe checks candidate CLI behavior with synthetic data only.
// It has no Docker, HTTP handler, persistent store or account dependency.
package agentprobe

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/internal/usage"
)

//go:embed probe.cjs
var Script string

const Version = 1

var Required = []string{"claude_turn", "claude_resume", "claude_interrupt", "claude_mcp", "claude_usage", "codex_handshake", "codex_turn", "codex_resume", "codex_interrupt", "codex_exec", "codex_usage"}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}
type Report struct {
	Stage     string  `json:"stage,omitempty"`
	Version   int     `json:"version"`
	ImageID   string  `json:"image_id"`
	CheckedAt int64   `json:"checked_at"`
	Checks    []Check `json:"checks"`
	Failure   string  `json:"failure,omitempty"`
}

func (r Report) Passed(imageID string) bool {
	if imageID == "" || r.Version != Version || r.ImageID != imageID || r.CheckedAt <= 0 || r.Failure != "" || len(r.Checks) != len(Required) {
		return false
	}
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if !c.Passed || seen[c.Name] {
			return false
		}
		seen[c.Name] = true
	}
	for _, name := range Required {
		if !seen[name] {
			return false
		}
	}
	return true
}

// Input uses the actual product argv, including its PID wrapper. The only
// substitution is a strictly validated synthetic session ID for Claude resume.
func Input() []byte {
	claude, _ := agent.ChatCommand(config.AgentClaude, "bypassPermissions", "", "claude-sonnet-4-6", "")
	resume, _ := agent.ChatCommand(config.AgentClaude, "bypassPermissions", "AGENTBOX_PROBE_RESUME", "claude-sonnet-4-6", "")
	codex, _ := agent.ChatCommand(config.AgentCodex, "bypassPermissions", "", "gpt-5.5", "")
	raw, _ := json.Marshal(map[string]any{"claude": claude, "claude_resume": resume, "codex": codex})
	return raw
}

type record struct {
	ExitCode *int              `json:"exit_code"`
	Signal   string            `json:"signal"`
	Name     string            `json:"name"`
	Agent    string            `json:"agent"`
	Lines    []json.RawMessage `json:"lines"`
	RPC      []json.RawMessage `json:"rpc"`
	Resume   string            `json:"resume"`
	Requests int64             `json:"requests"`
}

// Validate replays real captured CLI output through the same adapter and usage
// parser as chat. No probe can pass merely by claiming boolean success.
func Validate(ctx context.Context, imageID string, raw []byte) Report {
	result := Report{Version: Version, ImageID: imageID, CheckedAt: time.Now().UnixMilli()}
	fail := func(code string) Report { result.Failure = code; return result }
	if ctx.Err() != nil {
		return fail("probe_cancelled")
	}
	var output struct {
		Version int      `json:"version"`
		Checks  []string `json:"checks"`
		Records []record `json:"records"`
		Failed  string   `json:"failed"`
	}
	if len(raw) > 4<<20 || json.Unmarshal(raw, &output) != nil || output.Version != Version {
		return fail("probe_protocol_invalid")
	}
	if output.Failed != "" {
		for _, name := range Required {
			if output.Failed == name {
				result.Stage = name
			}
		}
		return fail("cli_behavior_failed")
	}
	seen := map[string]bool{}
	for _, name := range output.Checks {
		if seen[name] {
			return fail("probe_protocol_invalid")
		}
		seen[name] = true
	}
	for _, name := range Required {
		if name != "claude_usage" && name != "codex_usage" && !seen[name] {
			return fail("probe_incomplete")
		}
	}
	want := map[string]string{"claude_turn": "claude", "claude_resume": "claude", "claude_interrupt": "claude", "codex_turn": "codex", "codex_resume": "codex", "codex_interrupt": "codex", "codex_exec": "codex"}
	if len(output.Records) != len(want) {
		return fail("probe_incomplete")
	}
	for _, r := range output.Records {
		if want[r.Name] != r.Agent || r.Requests < 1 || r.Requests > 8 {
			return fail("probe_protocol_invalid")
		}
		delete(want, r.Name)
		result.Stage = r.Name
		adapter, err := agent.Lookup(r.Agent)
		if err != nil {
			return fail("probe_protocol_invalid")
		}
		memory := &memoryUsage{}
		meter := usage.New(ctx, &config.Config{}, memory)
		tally := meter.NewTally()
		terminal := ""
		observe := func(line []byte) {
			if e, ok := adapter.Decode(line); ok && e.Terminal != "" {
				terminal = e.Terminal
			}
			tally.Observe(store.UsageEvent{Agent: r.Agent, Kind: store.UsageKindChat, Model: "fixture", TurnID: r.Name}, line)
		}
		if r.RPC != nil {
			lines := make([]string, len(r.RPC))
			for i, v := range r.RPC {
				lines[i] = string(v)
			}
			err = agent.RunCodexTurn(ctx, io.Discard, strings.NewReader(strings.Join(lines, "\n")+"\n"), func() {}, nil,
				agent.CodexTurn{Prompt: "synthetic", ThreadID: r.Resume, Model: "gpt-5.5", Cwd: "/workspace"}, observe)
			if err != nil {
				return fail("appserver_contract_failed")
			}
		} else {
			for _, line := range r.Lines {
				observe(line)
			}
		}
		if r.Name == "claude_interrupt" {
			if r.Signal != "SIGINT" && (r.ExitCode == nil || *r.ExitCode != 130) && terminal != "failed" {
				return fail("interrupt_contract_failed")
			}
			continue
		}
		if r.Name == "codex_interrupt" {
			if terminal != "interrupted" {
				return fail("interrupt_contract_failed")
			}
			continue
		}
		if r.RPC == nil && (r.ExitCode == nil || *r.ExitCode != 0 || r.Signal != "") {
			return fail("cli_exit_failed")
		}
		if terminal != "completed" {
			return fail("terminal_contract_failed")
		}
		meter.Flush(&tally, 0)
		var input, cached, outputTokens int64
		for _, row := range memory.rows {
			input += row.InputTokens
			cached += row.CacheReadTokens
			outputTokens += row.OutputTokens
		}
		expectedInput := int64(6)
		if r.Agent == "claude" {
			expectedInput = 10
		}
		if tally.SettlementError() != nil || input != expectedInput*r.Requests || cached != 4*r.Requests || outputTokens != 2*r.Requests {
			return fail("usage_contract_failed")
		}
	}
	if len(want) != 0 {
		return fail("probe_incomplete")
	}
	result.Stage = ""
	for _, name := range Required {
		result.Checks = append(result.Checks, Check{Name: name, Passed: true})
	}
	return result
}

// Capture normalization in memory. This cannot write to production usage or
// ledger tables, and the probe never receives a database or Config instance.
type memoryUsage struct{ rows []store.UsageEvent }

func (m *memoryUsage) InsertUsage(rows ...store.UsageEvent) error {
	m.rows = append(m.rows, rows...)
	return nil
}
func (m *memoryUsage) InsertUsageMessages(rows ...store.UsageEvent) (int, error) {
	m.rows = append(m.rows, rows...)
	return len(rows), nil
}
func (*memoryUsage) UpsertTerminalUsage(...store.UsageEvent) error {
	return errors.New("probe does not scan terminals")
}
func (*memoryUsage) All() []store.Session             { return nil }
func (*memoryUsage) Get(string) (store.Session, bool) { return store.Session{}, false }
