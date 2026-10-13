// Package delegation holds the pure parts of the main-agent → worker model
// hand-off: routing rules over operator-supplied tags, prompts, worker report
// extraction and the before/after file manifest. It never runs a CLI, reads
// credentials or decides authorization; the server does that per call.
package delegation

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"agentbox/internal/config"
)

// Limits that bound one main agent turn.
const (
	MaxCallsPerTurn = 8
	MaxTaskBytes    = 32 << 10
	MaxPaths        = 64
	MaxReportBytes  = 6 << 10
	MaxNoteBytes    = 1 << 10
)

// Worker is one roster entry after the server rechecked access and tags.
type Worker struct {
	Ref          string                 `json:"ref"`
	AccountID    string                 `json:"account_id"`
	AccountLabel string                 `json:"account_label"`
	Agent        string                 `json:"agent"`
	Model        string                 `json:"model"`
	ModelLabel   string                 `json:"model_label"`
	Tags         config.ModelDelegation `json:"tags"`
	PriceIn      float64                `json:"price_in,omitempty"`
	PriceOut     float64                `json:"price_out,omitempty"`
	Priced       bool                   `json:"priced"`
}

func (w Worker) Label() string {
	name := w.ModelLabel
	if name == "" {
		name = w.Model
	}
	return name
}

// Task is the main agent's description of one subtask.
type Task struct {
	Text     string   `json:"task"`
	Type     string   `json:"task_type"`
	Risks    []string `json:"risks"`
	Access   string   `json:"access"`
	Paths    []string `json:"paths,omitempty"`
	Continue string   `json:"continue,omitempty"`
}

// Normalize validates and canonicalizes a task from the tool call.
func (t *Task) Normalize() error {
	t.Text = strings.TrimSpace(t.Text)
	if t.Text == "" || len(t.Text) > MaxTaskBytes || !utf8.ValidString(t.Text) || strings.ContainsRune(t.Text, 0) {
		return fmt.Errorf("task must be 1..%d bytes of text", MaxTaskBytes)
	}
	if !config.ValidDelegationTaskType(t.Type) {
		return fmt.Errorf("task_type must be one of %s", strings.Join(config.DelegationTaskTypes, ", "))
	}
	var risks []string
	for _, r := range t.Risks {
		if !config.ValidDelegationRisk(r) {
			return fmt.Errorf("risks must be chosen from %s", strings.Join(config.DelegationRisks, ", "))
		}
		if !slices.Contains(risks, r) {
			risks = append(risks, r)
		}
	}
	sort.Strings(risks)
	t.Risks = risks
	if t.Risks == nil {
		t.Risks = []string{}
	}
	if t.Access == "" {
		t.Access = "write"
	}
	if t.Access != "read" && t.Access != "write" {
		return fmt.Errorf(`access must be "read" or "write"`)
	}
	if len(t.Paths) > MaxPaths {
		t.Paths = t.Paths[:MaxPaths]
	}
	for i, p := range t.Paths {
		if len(p) > 512 || !utf8.ValidString(p) {
			t.Paths[i] = ""
		}
	}
	return nil
}

// Rejects reports why a worker may not take a task ("" when it may).
func (w Worker) Rejects(t Task) string {
	if !w.Tags.Enabled {
		return "not_tagged"
	}
	if !slices.Contains(w.Tags.Can, t.Type) {
		return "task_type"
	}
	for _, r := range t.Risks {
		if slices.Contains(w.Tags.Avoid, r) {
			return "risk:" + r
		}
	}
	return ""
}

// Eligible applies the hard tag filters, keeping roster order.
func Eligible(workers []Worker, t Task) ([]Worker, map[string]string) {
	var ok []Worker
	rejected := map[string]string{}
	for _, w := range workers {
		if why := w.Rejects(t); why != "" {
			rejected[w.Ref] = why
			continue
		}
		ok = append(ok, w)
	}
	return ok, rejected
}

// Pick is the deterministic choice among several eligible workers: the
// economy tier first, then the lower known price, then roster order. A worker
// without a configured price sorts after priced ones of the same tier.
func Pick(workers []Worker) Worker {
	best := workers[0]
	for _, w := range workers[1:] {
		if better(w, best) {
			best = w
		}
	}
	return best
}

func better(a, b Worker) bool {
	if (a.Tags.Tier == "economy") != (b.Tags.Tier == "economy") {
		return a.Tags.Tier == "economy"
	}
	if a.Priced != b.Priced {
		return a.Priced
	}
	if a.Priced && a.PriceIn+a.PriceOut != b.PriceIn+b.PriceOut {
		return a.PriceIn+a.PriceOut < b.PriceIn+b.PriceOut
	}
	return false
}

func price(priced bool, in, out float64) string {
	if !priced {
		return "No configured price."
	}
	return fmt.Sprintf("Configured reference USD per million tokens: uncached input %g, output %g.", in, out)
}

// Description is the only business input Jev receives about a worker.
func (w Worker) Description() string {
	parts := []string{"Worker tier " + w.Tags.Tier + "."}
	parts = append(parts, "Can do task types: "+strings.Join(w.Tags.Can, ", ")+".")
	if len(w.Tags.Avoid) > 0 {
		parts = append(parts, "Should not take risks: "+strings.Join(w.Tags.Avoid, ", ")+".")
	}
	if w.Tags.Notes != "" {
		parts = append(parts, "Notes: "+w.Tags.Notes)
	}
	parts = append(parts, price(w.Priced, w.PriceIn, w.PriceOut))
	return truncate(strings.Join(parts, " "), 1000)
}

// SelfDescription describes the option of keeping the subtask.
func SelfDescription(priced bool, in, out float64) string {
	return "The strong main agent does the subtask itself; suitable for design decisions, ambiguous requirements and high-risk changes. " + price(priced, in, out)
}

// JevState is the subtask evidence sent with the routing question.
func JevState(t Task) string {
	risks := "none"
	if len(t.Risks) > 0 {
		risks = strings.Join(t.Risks, ", ")
	}
	return truncate(fmt.Sprintf("Subtask type: %s\nRisks: %s\nAccess: %s\nPaths mentioned: %d\nTask:\n%s", t.Type, risks, t.Access, len(t.Paths), t.Text), 12<<10)
}

// Route records how one call was routed; it is saved with the delegation.
type Route struct {
	Source     string            `json:"source"`
	Reason     string            `json:"reason"`
	Confidence float64           `json:"confidence,omitempty"`
	JevModel   string            `json:"jev_model,omitempty"`
	Eligible   []string          `json:"eligible"`
	Rejected   map[string]string `json:"rejected,omitempty"`
	Roster     []Worker          `json:"roster,omitempty"`
}

// truncate shortens text on a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Truncate is truncate for callers outside the package.
func Truncate(s string, max int) string { return truncate(s, max) }
