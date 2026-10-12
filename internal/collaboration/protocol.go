// Package collaboration owns provider-independent task roles, handoffs and
// workflow rules. Role names never imply a particular CLI, account or model.
package collaboration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"agentbox/internal/config"
)

const Version = 1
const MaxHandoffBytes = 64 << 10
const MaxInputBytes = 1 << 20

type Role string

const (
	Coordinator Role = "coordinator"
	Executor    Role = "executor"
)

var (
	ErrInvalid  = errors.New("invalid collaboration input")
	ErrConflict = errors.New("collaboration revision or input conflict")
	ErrGone     = errors.New("collaboration request was removed")
	ErrState    = errors.New("collaboration action is not valid in this state")
	keyPattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
	refPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

func ValidID(id string) bool   { return keyPattern.MatchString(id) }
func ValidRole(role Role) bool { return role == Coordinator || role == Executor }

type Binding struct {
	AgentType     string `json:"agent_type"`
	AccountID     string `json:"account_id"`
	Model         string `json:"model"`
	Effort        string `json:"effort"`
	EffortControl string `json:"effort_control"`
}

type Roles struct {
	Coordinator Binding `json:"coordinator"`
	Executor    Binding `json:"executor"`
}

func (r Roles) Binding(role Role) (Binding, bool) {
	switch role {
	case Coordinator:
		return r.Coordinator, true
	case Executor:
		return r.Executor, true
	default:
		return Binding{}, false
	}
}

// Validate checks shape only. Every admission and exec must ALSO recheck the
// user's account access, model allowlist and current reasoning capabilities.
func (r Roles) Validate() error {
	for _, b := range []Binding{r.Coordinator, r.Executor} {
		if (b.AgentType != config.AgentClaude && b.AgentType != config.AgentCodex) ||
			!refPattern.MatchString(b.AccountID) || !config.ValidModelID(b.Model) ||
			len(b.Effort) > 16 || (b.EffortControl != "" && b.EffortControl != "effort" && b.EffortControl != "budget") {
			return ErrInvalid
		}
		if b.Effort != "" && !refPattern.MatchString(b.Effort) {
			return ErrInvalid
		}
	}
	return nil
}

type PlanItem struct {
	ID         string   `json:"id"`
	Text       string   `json:"text"`
	Acceptance []string `json:"acceptance"`
}

type Plan struct {
	Summary     string         `json:"summary"`
	Items       []PlanItem     `json:"items"`
	Constraints []string       `json:"constraints"`
	CodeMap     []CodeMapEntry `json:"code_map,omitempty"`
}

// CodeMapEntry transfers the planner's exploration without copying its raw
// provider thread. References are hints: the executor still checks current
// files before editing, and excerpts never grant permissions.
type CodeMapEntry struct {
	Path        string   `json:"path"`
	Symbols     []string `json:"symbols,omitempty"`
	Purpose     string   `json:"purpose"`
	Constraints []string `json:"constraints,omitempty"`
	Excerpt     string   `json:"excerpt,omitempty"`
}

type Question struct {
	Text        string   `json:"text"`
	Options     []string `json:"options"`
	Blocking    bool     `json:"blocking"`
	ContextRefs []string `json:"context_refs"`
}

type Answer struct {
	QuestionID string `json:"question_id"`
	Text       string `json:"text"`
}

type Result struct {
	Summary   string   `json:"summary"`
	Completed []string `json:"completed"`
	Remaining []string `json:"remaining"`
	Tests     []string `json:"tests"` // claims, not platform verification evidence
}

type Review struct {
	Decision    string   `json:"decision"`
	SnapshotID  string   `json:"snapshot_id"`
	PlanVersion int64    `json:"plan_version"`
	Summary     string   `json:"summary"`
	Changes     []string `json:"changes"`
}

// Handoff is model-authored content only. Trusted sender, run/step identifiers,
// versions and ordering belong to the server's message envelope.
type Handoff struct {
	Version int             `json:"version"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func ParseHandoff(raw []byte, role Role) (Handoff, error) {
	var h Handoff
	if !ValidRole(role) || Decode(raw, &h, MaxHandoffBytes) != nil || h.Version != Version {
		return h, ErrInvalid
	}
	valid := false
	switch h.Type {
	case "plan":
		var p Plan
		if role != Coordinator || Decode(h.Payload, &p, MaxHandoffBytes) != nil || !validText(p.Summary, 8192) || len(p.Items) < 1 || len(p.Items) > 32 || !texts(p.Constraints, 32) || !validCodeMap(p.CodeMap) {
			break
		}
		seen := map[string]bool{}
		valid = true
		for _, item := range p.Items {
			if !refPattern.MatchString(item.ID) || seen[item.ID] || !validText(item.Text, 8192) || len(item.Acceptance) < 1 || !texts(item.Acceptance, 16) {
				valid = false
			}
			seen[item.ID] = true
		}
	case "question":
		var p Question
		valid = Decode(h.Payload, &p, MaxHandoffBytes) == nil && validText(p.Text, 8192) && p.Blocking && texts(p.Options, 8) && len(p.ContextRefs) <= 32
		for _, ref := range p.ContextRefs {
			valid = valid && refPattern.MatchString(ref)
		}
	case "answer":
		var p Answer
		valid = role == Coordinator && Decode(h.Payload, &p, MaxHandoffBytes) == nil && ValidID(p.QuestionID) && validText(p.Text, 16384)
	case "result":
		var p Result
		valid = role == Executor && Decode(h.Payload, &p, MaxHandoffBytes) == nil && validText(p.Summary, 8192) && texts(p.Completed, 32) && texts(p.Remaining, 32) && texts(p.Tests, 32)
	case "review":
		var p Review
		if role != Coordinator || Decode(h.Payload, &p, MaxHandoffBytes) != nil || !ValidID(p.SnapshotID) || p.PlanVersion < 1 || !validText(p.Summary, 8192) || !texts(p.Changes, 32) {
			break
		}
		valid = (p.Decision == "approve" && len(p.Changes) == 0) || (p.Decision == "revise" && len(p.Changes) > 0) || p.Decision == "needs_user"
	}
	if !valid {
		return h, ErrInvalid
	}
	return h, nil
}

func validText(s string, max int) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= max && !strings.ContainsRune(s, 0)
}
func texts(values []string, max int) bool {
	if len(values) > max {
		return false
	}
	for _, value := range values {
		if !validText(value, 8192) {
			return false
		}
	}
	return true
}

// Decode rejects duplicate keys (including escaped aliases), trailing JSON,
// excessive depth, null roots and unknown fields. Nothing is salvaged from
// prose, markdown fences, tool output or an incomplete streaming fragment.
func Decode(raw []byte, dst any, maxBytes int) error {
	if len(raw) == 0 || len(raw) > maxBytes || !utf8.Valid(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if uniqueValue(d, 0) != nil {
		return ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil {
		return ErrInvalid
	}
	return nil
}

func uniqueValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || keys[name] {
				return ErrInvalid
			}
			keys[name] = true
			if uniqueValue(d, depth+1) != nil {
				return ErrInvalid
			}
		}
	case '[':
		for d.More() {
			if uniqueValue(d, depth+1) != nil {
				return ErrInvalid
			}
		}
	default:
		return ErrInvalid
	}
	end, err := d.Token()
	if err != nil || (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
		return ErrInvalid
	}
	return nil
}
