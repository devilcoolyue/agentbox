// Package decision provides bounded advice. It never authorizes an Agent turn,
// mutates an execution binding, or owns provider credentials in task inputs.
package decision

import (
	"context"
	"errors"
)

// TemplateVersion names the only question wording an adapter may send. A new
// wording needs a new version so recorded routes stay interpretable.
const TemplateVersion = "delegation-worker-choice-v1"

// SelfRef is the fixed option meaning "the main agent keeps this subtask".
const SelfRef = "self"

// Candidate descriptions are the provider's only business input about an
// option; account IDs, model bindings and credentials never leave the server.
type Candidate struct {
	Ref         string `json:"ref"`
	Description string `json:"description"`
}

// Request carries one or more independent choice questions keyed by a short
// name. Each question is answered on its own; one invalid answer does not
// discard another.
type Request struct {
	ID              string                 `json:"id"`
	TemplateVersion string                 `json:"template_version"`
	State           string                 `json:"state"`
	Questions       map[string][]Candidate `json:"questions"`
}
type Suggestion struct {
	Status       string  `json:"status"`
	CandidateRef string  `json:"candidate_ref,omitempty"`
	Reason       string  `json:"reason"`
	Confidence   float64 `json:"confidence"`
}

// Known describes token evidence, not whether a request was free. CostStatus is
// known, unknown or not_dispatched.
type Usage struct {
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
	Known        bool   `json:"known"`
	CostStatus   string `json:"cost_status"`
}
type Evaluation struct {
	Provider       string                `json:"provider"`
	Model          string                `json:"model,omitempty"`
	Status         string                `json:"status"`
	ScoreSemantics string                `json:"score_semantics,omitempty"`
	Suggestions    map[string]Suggestion `json:"suggestions"`
	Usage          Usage                 `json:"usage"`
	Dispatched     bool                  `json:"dispatched"`
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
