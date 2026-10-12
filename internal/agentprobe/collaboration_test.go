package agentprobe

import (
	"encoding/json"
	"strings"
	"testing"
)

func collaborationFixture() map[string]any {
	var spec struct {
		Handoffs map[string]json.RawMessage `json:"handoffs"`
	}
	_ = json.Unmarshal(CollaborationInput(), &spec)
	var records []map[string]any
	for _, provider := range []string{"claude", "codex"} {
		for _, kind := range []string{"plan", "result"} {
			rec := map[string]any{"agent": provider, "name": provider + "_" + kind, "requests": 1, "exit_code": 0}
			resume := ""
			rpcID := 3
			session := "synthetic-thread"
			if kind == "result" {
				resume = session
				rpcID = 2
			}
			rec["resume"] = resume
			if provider == "claude" {
				rec["lines"] = []any{map[string]any{"type": "result", "subtype": "success", "session_id": session, "result": string(spec.Handoffs[kind])}}
			} else {
				rec["rpc"] = []any{
					map[string]any{"id": 1, "result": map[string]any{}},
					map[string]any{"id": rpcID, "result": map[string]any{"thread": map[string]any{"id": session}}},
					map[string]any{"id": 4, "result": map[string]any{"turn": map[string]any{"id": "synthetic-turn"}}},
					map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]any{"type": "agentMessage", "text": string(spec.Handoffs[kind]), "phase": "final_answer", "id": "m1"}}},
					map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"status": "completed"}}},
				}
			}
			records = append(records, rec)
		}
	}
	return map[string]any{"version": 1, "records": records}
}

func TestCollaborationProbeRequiresActualFinalAndResumeEvidence(t *testing.T) {
	for _, mutation := range []string{"valid", "claimed_checks_only", "missing_resume", "wrong_role", "duplicate_provider", "tool_text", "failed_terminal"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := collaborationFixture()
			records := fixture["records"].([]map[string]any)
			switch mutation {
			case "claimed_checks_only":
				fixture["records"] = nil
				fixture["checks"] = CollaborationRequired
			case "missing_resume":
				records[1]["resume"] = ""
			case "wrong_role":
				records[0]["lines"] = records[1]["lines"]
			case "duplicate_provider":
				records[2] = records[0]
			case "tool_text":
				items := records[2]["rpc"].([]any)
				items[3].(map[string]any)["params"].(map[string]any)["item"].(map[string]any)["type"] = "commandExecution"
			case "failed_terminal":
				items := records[2]["rpc"].([]any)
				items[4].(map[string]any)["params"].(map[string]any)["turn"].(map[string]any)["status"] = "failed"
			}
			raw, _ := json.Marshal(fixture)
			image := "sha256:" + strings.Repeat("a", 64)
			report := ValidateCollaboration(t.Context(), image, raw)
			if report.CollaborationPassed(image) != (mutation == "valid") {
				t.Fatal(report)
			}
		})
	}
}
