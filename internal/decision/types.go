// Package decision provides bounded advice. It never authorizes an Agent turn,
// mutates an execution binding, or owns provider credentials in task inputs.
package decision

import (
	"context"
	"errors"

	"agentbox/internal/collaboration"
)

const Version = 1
const TemplateVersion = "collaboration-model-choice-v1"

type Intent struct {
	Version  int                               `json:"version"`
	Provider string                            `json:"provider"`
	Roles    map[collaboration.Role]RoleIntent `json:"roles"`
}
type RoleIntent struct {
	Mode string `json:"mode"`
}
type Candidate struct {
	Ref         string                `json:"ref"`
	Binding     collaboration.Binding `json:"binding"`
	Description string                `json:"description"`
}
type RoleSnapshot struct {
	Mode       string                `json:"mode"`
	Binding    collaboration.Binding `json:"binding"`
	Candidates []Candidate           `json:"candidates,omitempty"`
	Fallback   collaboration.Binding `json:"fallback"`
}
type Snapshot struct {
	Version          int                                 `json:"version"`
	TemplateVersion  string                              `json:"template_version,omitempty"`
	CandidateVersion string                              `json:"candidate_version,omitempty"`
	ConfigRevision   int64                               `json:"config_revision"`
	Provider         string                              `json:"provider"`
	Roles            map[collaboration.Role]RoleSnapshot `json:"roles"`
}
type Decision struct {
	Binding collaboration.Binding `json:"binding"`
	Source  string                `json:"source"`
	Reason  string                `json:"reason"`
}
type Result struct {
	Roles         collaboration.Roles             `json:"roles"`
	Decisions     map[collaboration.Role]Decision `json:"decisions"`
	ProviderModel string                          `json:"provider_model,omitempty"`
}
type Request struct {
	ID              string                             `json:"id"`
	Purpose         string                             `json:"purpose"`
	TemplateVersion string                             `json:"template_version"`
	State           string                             `json:"state"`
	Roles           map[collaboration.Role][]Candidate `json:"roles"`
}
type Suggestion struct {
	Status       string  `json:"status"`
	CandidateRef string  `json:"candidate_ref,omitempty"`
	Reason       string  `json:"reason"`
	Confidence   float64 `json:"confidence"`
}

// Known describes token evidence, not whether a request was free. CostStatus is
// known, unknown or not_dispatched. Unknown evidence must retain reservations.
type Usage struct {
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
	Known        bool   `json:"known"`
	CostStatus   string `json:"cost_status"`
}
type Evaluation struct {
	Provider       string                            `json:"provider"`
	Model          string                            `json:"model,omitempty"`
	Status         string                            `json:"status"`
	ScoreSemantics string                            `json:"score_semantics,omitempty"`
	Suggestions    map[collaboration.Role]Suggestion `json:"suggestions"`
	Usage          Usage                             `json:"usage"`
	Dispatched     bool                              `json:"dispatched"`
}
type Capabilities struct {
	Choice bool `json:"choice"`
	Score  bool `json:"score"`
	Noul   bool `json:"noul"`
}
type Provider interface {
	ID() string
	Capabilities() Capabilities
	Evaluate(context.Context, Request) (Evaluation, error)
}

var (
	ErrInvalid       = errors.New("invalid decision request")
	ErrUnavailable   = errors.New("decision provider unavailable")
	ErrBudgetUnknown = errors.New("decision cost bound unavailable")
	ErrBusy          = errors.New("decision concurrency limit reached")
	ErrCircuitOpen   = errors.New("decision provider circuit open")
	ErrGeneration    = errors.New("decision configuration changed")
)

func (i Intent) Validate() error {
	if i.Version != Version || (i.Provider != "inherit" && i.Provider != "builtin" && i.Provider != "jev") || len(i.Roles) != 2 {
		return ErrInvalid
	}
	for _, role := range []collaboration.Role{collaboration.Coordinator, collaboration.Executor} {
		r, ok := i.Roles[role]
		if !ok || (r.Mode != "fixed" && r.Mode != "auto") {
			return ErrInvalid
		}
	}
	return nil
}
