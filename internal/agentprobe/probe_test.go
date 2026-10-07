package agentprobe

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixture() map[string]any {
	checks := []string{}
	for _, name := range Required {
		if !strings.HasSuffix(name, "_usage") {
			checks = append(checks, name)
		}
	}
	records := []map[string]any{}
	for _, name := range []string{"claude_turn", "claude_resume", "claude_interrupt"} {
		exitCode := 0
		if name == "claude_interrupt" {
			exitCode = 130
		}
		records = append(records, map[string]any{"name": name, "agent": "claude", "requests": 1, "exit_code": exitCode, "lines": []any{
			map[string]any{"type": "assistant", "message": map[string]any{"id": "synthetic", "model": "claude-sonnet-4-6", "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "cache_read_input_tokens": 4, "output_tokens": 2}}},
			map[string]any{"type": "result", "subtype": "success"},
		}})
	}
	for _, name := range []string{"codex_turn", "codex_resume", "codex_interrupt"} {
		status := "completed"
		if name == "codex_interrupt" {
			status = "interrupted"
		}
		records = append(records, map[string]any{"name": name, "agent": "codex", "requests": 1, "rpc": []any{
			map[string]any{"id": 1, "result": map[string]any{}},
			map[string]any{"id": 3, "result": map[string]any{"thread": map[string]string{"id": "synthetic-thread"}}},
			map[string]any{"id": 4, "result": map[string]any{"turn": map[string]string{"id": "synthetic-turn"}}},
			map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"last": map[string]int{"inputTokens": 10, "cachedInputTokens": 4, "outputTokens": 2}}}},
			map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"id": "synthetic-turn", "status": status}}},
		}})
	}
	records = append(records, map[string]any{"name": "codex_exec", "agent": "codex", "requests": 1, "exit_code": 0, "lines": []any{map[string]any{"type": "turn.completed", "usage": map[string]int{"input_tokens": 10, "cached_input_tokens": 4, "output_tokens": 2, "reasoning_output_tokens": 1}}}})
	return map[string]any{"version": 1, "checks": checks, "records": records}
}
func TestProbeRejectsChangedProtocolsAndUsage(t *testing.T) {
	for _, kind := range []string{"ok", "handshake", "usage", "terminal", "missing", "duplicate", "foreign", "failure", "interrupt"} {
		t.Run(kind, func(t *testing.T) {
			data := fixture()
			records := data["records"].([]map[string]any)
			switch kind {
			case "handshake":
				records[3]["rpc"] = []any{}
			case "usage":
				records[6]["lines"].([]any)[0].(map[string]any)["usage"] = map[string]int{"future_input_tokens": 10, "output_tokens": 2}
			case "terminal":
				records[0]["lines"].([]any)[1].(map[string]any)["subtype"] = "future_success"
			case "missing":
				data["checks"] = []string{"claude_turn"}
			case "duplicate":
				records[6] = records[0]
			case "foreign":
				records[0]["agent"] = "codex"
			case "interrupt":
				records[2]["exit_code"] = 0
			case "failure":
				data["failed"] = "claude_mcp"
			}
			raw, _ := json.Marshal(data)
			report := Validate(t.Context(), "fixture-id", raw)
			if report.Passed("fixture-id") != (kind == "ok") {
				t.Fatalf("unexpected result %+v", report)
			}
			if report.Passed("another-image") {
				t.Fatal("cross-image evidence accepted")
			}
			if kind == "usage" && report.Failure != "usage_contract_failed" {
				t.Fatal(report)
			}
		})
	}
}
func TestProbeReportRequiresCurrentCompleteEvidence(t *testing.T) {
	raw, _ := json.Marshal(fixture())
	report := Validate(t.Context(), "fixture-id", raw)
	report.Version++
	if report.Passed("fixture-id") {
		t.Fatal("future report accepted")
	}
	report.Version = Version
	report.Checks[0].Passed = false
	if report.Passed("fixture-id") {
		t.Fatal("failed check accepted")
	}
}
