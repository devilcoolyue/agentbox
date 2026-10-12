package collaboration

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanCodeMapIsOptionalBoundedAndRelative(t *testing.T) {
	base := Plan{Summary: "Fix calculation", Items: []PlanItem{{ID: "fix", Text: "Fix total", Acceptance: []string{"regression passes"}}}}
	parse := func(p Plan) error {
		body, _ := json.Marshal(p)
		raw, _ := json.Marshal(Handoff{Version: 1, Type: "plan", Payload: body})
		_, err := ParseHandoff(raw, Coordinator)
		return err
	}
	if err := parse(base); err != nil {
		t.Fatal("old plans rejected", err)
	}
	base.CodeMap = []CodeMapEntry{{Path: "src/total.ts", Symbols: []string{"total"}, Purpose: "Computes order total", Constraints: []string{"Round only once"}, Excerpt: "export function total()"}}
	if err := parse(base); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/etc/passwd", "../secret", "src/../../secret", ".git/config", "src/.git/config", "C:/secret", "src\\secret", ".", "src/./file", "bad\npath", strings.Repeat("a", 513)} {
		p := base
		p.CodeMap = append([]CodeMapEntry(nil), base.CodeMap...)
		p.CodeMap[0].Path = path
		if err := parse(p); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
	for _, mutation := range []func(*Plan){
		func(p *Plan) { p.CodeMap = append(p.CodeMap, p.CodeMap[0]) },
		func(p *Plan) { p.CodeMap[0].Symbols = []string{strings.Repeat("a", 257)} },
		func(p *Plan) { p.CodeMap[0].Purpose = strings.Repeat("a", 1025) },
		func(p *Plan) { p.CodeMap[0].Constraints = []string{strings.Repeat("a", 1025)} },
		func(p *Plan) { p.CodeMap[0].Excerpt = strings.Repeat("a", 2049) },
		func(p *Plan) { p.CodeMap[0].Symbols = make([]string, 17) },
		func(p *Plan) { p.CodeMap[0].Constraints = make([]string, 9) },
		func(p *Plan) { p.CodeMap = make([]CodeMapEntry, 33) },
	} {
		p := base
		p.CodeMap = append([]CodeMapEntry(nil), base.CodeMap...)
		mutation(&p)
		if err := parse(p); err == nil {
			t.Fatal("accepted excessive code map")
		}
	}
}

func TestWorkflowPromptPinsEvidenceAndKeepsStepInstructionsFocused(t *testing.T) {
	id := strings.Repeat("b", 32)
	workflow := &Workflow{Version: 1, Mode: "reviewed"}
	if _, err := BuildPromptWithOptions(Coordinator, "review", "goal", id, 1, nil, PromptOptions{Workflow: workflow}); err == nil {
		t.Fatal("new review accepted without evidence")
	}
	// Legacy queued workflows remain readable and can create legacy review inputs.
	if _, err := BuildPrompt(Coordinator, "review", "goal", id, 1, nil); err != nil {
		t.Fatal(err)
	}
	empty, _ := json.Marshal([]FileChange{})
	e := ReviewEvidence{Version: 1, BaselineID: strings.Repeat("a", 32), BaselineSHA256: strings.Repeat("a", 64), SnapshotID: id, SnapshotSHA256: strings.Repeat("b", 64), PlanVersion: 1, ChangesSHA256: reviewDigest(empty), Changes: []ReviewFileEvidence{}, Complete: true, Limits: reviewEvidenceLimits}
	opts := PromptOptions{Workflow: workflow, ReviewEvidence: &e, VerificationCommand: "npm test"}
	prompt, err := BuildPromptWithOptions(Coordinator, "review", "goal", id, 1, nil, opts)
	if err != nil || !strings.Contains(prompt, `"review_evidence"`) || !strings.Contains(prompt, "Failed verification requires revise or needs_user, never approve") || !strings.Contains(prompt, "omission_reasons") {
		t.Fatal(prompt, err)
	}
	opts.ReviewEvidence.PlanVersion = 2
	if _, err := BuildPromptWithOptions(Coordinator, "review", "goal", id, 1, nil, opts); err == nil {
		t.Fatal("different plan evidence accepted")
	}
	prompt, err = BuildPromptWithOptions(Executor, "execute", "goal", id, 0, nil, PromptOptions{Workflow: &Workflow{Version: 1, Mode: "direct"}, VerificationCommand: "npm test"})
	if err != nil || !strings.Contains(prompt, "Run verification_command") || !strings.Contains(prompt, `"verification_command":"npm test"`) || !strings.Contains(prompt, "In direct mode") || !strings.Contains(prompt, "previous_platform_verification is diagnostic evidence for its stated earlier snapshot only") || strings.Contains(prompt, "review: {") || strings.Contains(prompt, "omitted_changes") {
		t.Fatal(prompt, err)
	}
	var absent *Workflow
	if absent.IsDirect() || (&Workflow{Version: 2, Mode: "direct"}).Validate() == nil || (&Workflow{Version: 1, Mode: "auto"}).Validate() == nil {
		t.Fatal("unknown workflow accepted")
	}
}
