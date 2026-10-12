package decision

import (
	"agentbox/internal/collaboration"
	"context"
)

// Builtin intentionally does not infer model capability from names or labels.
// A unique authorized candidate is the only unambiguous automatic rule until
// the operator has supplied measured capability policies.
type Builtin struct{}

func (Builtin) ID() string                 { return "builtin" }
func (Builtin) Capabilities() Capabilities { return Capabilities{Choice: true} }
func (Builtin) Resolve(role RoleSnapshot) (collaboration.Binding, string, bool) {
	if role.Mode == "fixed" {
		return role.Binding, "fixed_model", true
	}
	if role.Mode == "auto" && len(role.Candidates) == 1 {
		return role.Candidates[0].Binding, "unique_candidate", true
	}
	return collaboration.Binding{}, "ambiguous_candidates", false
}
func (Builtin) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	out := Evaluation{Provider: "builtin", Status: "abstain", Usage: Usage{CostStatus: "not_dispatched"}, Suggestions: make(map[collaboration.Role]Suggestion)}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	for role, candidates := range req.Roles {
		s := Suggestion{Status: "abstain", Reason: "ambiguous_candidates"}
		if len(candidates) == 1 {
			s = Suggestion{Status: "suggest", CandidateRef: candidates[0].Ref, Reason: "unique_candidate", Confidence: 1}
			out.Status = "suggest"
		}
		out.Suggestions[role] = s
	}
	return out, nil
}
