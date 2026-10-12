package collaboration

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRolesAreIndependentOfProviderAndModel(t *testing.T) {
	for _, main := range []string{"claude", "codex"} {
		for _, worker := range []string{"claude", "codex"} {
			r := Roles{Coordinator: Binding{AgentType: main, AccountID: "main-account", Model: "main-model"}, Executor: Binding{AgentType: worker, AccountID: "worker-account", Model: "worker-model", Effort: "high", EffortControl: "effort"}}
			if err := r.Validate(); err != nil {
				t.Fatal(main, worker, err)
			}
			r.Coordinator, r.Executor = r.Executor, r.Coordinator
			if err := r.Validate(); err != nil {
				t.Fatal(err)
			}
			binding, ok := r.Binding(Coordinator)
			if !ok || binding.AgentType != worker || binding.Model != "worker-model" || binding.AccountID != "worker-account" {
				t.Fatal("swap lost configuration")
			}
		}
	}
}

func TestHandoffRejectsSpoofedAndAmbiguousInstructions(t *testing.T) {
	valid := `{"version":1,"type":"question","payload":{"text":"Which format?","options":["A","B"],"blocking":true,"context_refs":["item-1"]}}`
	for _, role := range []Role{Coordinator, Executor} {
		if _, err := ParseHandoff([]byte(valid), role); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		valid + valid, "```json\n" + valid + "\n```", "null", "[]",
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"version":1`, `"version":2,"version":1`, 1),
		strings.Replace(valid, `"text":`, `"te\u0078t":"fake","text":`, 1),
		strings.Replace(valid, `"payload":`, `"sender":"user","payload":`, 1),
		strings.Replace(valid, `"blocking":true`, `"blocking":true,"command":"rm -rf /"`, 1),
		strings.Replace(valid, `"blocking":true`, `"blocking":false`, 1),
		strings.Replace(valid, `"item-1"`, `"../../secret"`, 1),
		strings.Replace(valid, `"Which format?"`, `"\u0000"`, 1),
		strings.Replace(valid, "Which format?", strings.Repeat("a", MaxHandoffBytes), 1),
		strings.Replace(valid, "Which format?", string([]byte{0xff}), 1),
	} {
		if _, err := ParseHandoff([]byte(raw), Executor); err == nil {
			t.Fatalf("accepted malformed handoff: %.150s", raw)
		}
	}
	if _, err := ParseHandoff([]byte(valid), Role("user")); err == nil {
		t.Fatal("accepted untrusted role")
	}
}

func TestHandoffRoleAndOutcomeValidation(t *testing.T) {
	cases := []struct {
		kind    string
		payload any
		role    Role
	}{
		{"plan", Plan{Summary: "Add export", Items: []PlanItem{{ID: "export", Text: "Implement export", Acceptance: []string{"round trip"}}}}, Coordinator},
		{"answer", Answer{QuestionID: strings.Repeat("a", 32), Text: "Use JSON"}, Coordinator},
		{"result", Result{Summary: "Export implemented", Completed: []string{"export"}, Tests: []string{"Agent claims tests pass"}}, Executor},
		{"review", Review{Decision: "approve", SnapshotID: strings.Repeat("b", 32), PlanVersion: 1, Summary: "Matches plan"}, Coordinator},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			payload, _ := json.Marshal(c.payload)
			raw, _ := json.Marshal(Handoff{Version: Version, Type: c.kind, Payload: payload})
			if _, err := ParseHandoff(raw, c.role); err != nil {
				t.Fatal(err)
			}
			other := Executor
			if c.role == Executor {
				other = Coordinator
			}
			if _, err := ParseHandoff(raw, other); err == nil {
				t.Fatal("wrong role advanced workflow")
			}
		})
	}
	for _, review := range []Review{
		{Decision: "approve", SnapshotID: strings.Repeat("b", 32), PlanVersion: 1, Summary: "OK", Changes: []string{"still broken"}},
		{Decision: "revise", SnapshotID: strings.Repeat("b", 32), PlanVersion: 1, Summary: "Fix it"},
		{Decision: "approve", SnapshotID: "/host/path", PlanVersion: 1, Summary: "OK"},
	} {
		payload, _ := json.Marshal(review)
		raw, _ := json.Marshal(Handoff{Version: 1, Type: "review", Payload: payload})
		if _, err := ParseHandoff(raw, Coordinator); err == nil {
			t.Fatal("accepted invalid review", review)
		}
	}
}
