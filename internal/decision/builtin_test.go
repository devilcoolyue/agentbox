package decision

import (
	"agentbox/internal/collaboration"
	"context"
	"testing"
)

func TestBuiltinPreservesFixedAndOnlyResolvesUniqueCandidate(t *testing.T) {
	b := Builtin{}
	fixed := collaboration.Binding{AgentType: "codex", AccountID: "a", Model: "fixed", Effort: "high"}
	if got, reason, ok := b.Resolve(RoleSnapshot{Mode: "fixed", Binding: fixed, Candidates: []Candidate{{Binding: collaboration.Binding{Model: "other"}}}}); !ok || got != fixed || reason != "fixed_model" {
		t.Fatal(got, reason, ok)
	}
	if got, reason, ok := b.Resolve(RoleSnapshot{Mode: "auto", Candidates: []Candidate{{Binding: fixed}}}); !ok || got != fixed || reason != "unique_candidate" {
		t.Fatal(got, reason, ok)
	}
	if _, _, ok := b.Resolve(RoleSnapshot{Mode: "auto", Candidates: []Candidate{{Description: "strongest expensive model", Binding: fixed}, {Description: "cheap model"}}}); ok {
		t.Fatal("model labels must not create a quality policy")
	}
	e, err := b.Evaluate(context.Background(), testRequest())
	if err != nil || e.Dispatched || e.Status != "abstain" || e.Usage.CostStatus != "not_dispatched" {
		t.Fatal(e, err)
	}
}
func TestIntentRequiresBothValidRoles(t *testing.T) {
	i := Intent{Version: 1, Provider: "inherit", Roles: map[collaboration.Role]RoleIntent{collaboration.Coordinator: {Mode: "fixed"}, collaboration.Executor: {Mode: "auto"}}}
	if err := i.Validate(); err != nil {
		t.Fatal(err)
	}
	i.Roles["rogue"] = RoleIntent{Mode: "auto"}
	if i.Validate() == nil {
		t.Fatal("extra role accepted")
	}
	delete(i.Roles, "rogue")
	delete(i.Roles, collaboration.Executor)
	if i.Validate() == nil {
		t.Fatal("missing role accepted")
	}
}
