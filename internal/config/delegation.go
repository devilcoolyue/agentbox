package config

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// DelegationVocabularyVersion versions the task types and risks below. The
// main agent classifies each delegate call with the same words the operator
// used to tag models, so routing never guesses capability from model names.
const DelegationVocabularyVersion = 1

// DelegationTaskTypes is what a worker can be asked to do, in display order.
var DelegationTaskTypes = []string{
	"search", "summarize", "mechanical_edit", "implement_spec", "write_tests",
	"fix_tests", "ui_copy", "docs", "refactor_local", "other",
}

// DelegationRisks are properties of a subtask that a worker may be tagged to
// avoid. "other" in task types is never routed by the default rules unless a
// model explicitly lists it.
var DelegationRisks = []string{
	"auth", "billing", "migration", "concurrency", "security",
	"data_loss", "public_api", "cross_module", "ambiguous_spec",
}

// ModelDelegation tags one account model as a possible worker. It is an
// operator's claim about measured fitness, not something inferred.
type ModelDelegation struct {
	Enabled bool     `json:"enabled"`
	Tier    string   `json:"tier,omitempty"`
	Can     []string `json:"can,omitempty"`
	Avoid   []string `json:"avoid,omitempty"`
	Notes   string   `json:"notes,omitempty"`
}

func ValidDelegationTaskType(v string) bool { return slices.Contains(DelegationTaskTypes, v) }
func ValidDelegationRisk(v string) bool     { return slices.Contains(DelegationRisks, v) }

func (d *ModelDelegation) normalize() error {
	if d == nil {
		return nil
	}
	if d.Tier == "" {
		d.Tier = "economy"
	}
	if d.Tier != "economy" && d.Tier != "standard" {
		return fmt.Errorf("跑腿档位必须是 economy 或 standard")
	}
	clean := func(values []string, valid func(string) bool, field string) ([]string, error) {
		var out []string
		for _, v := range values {
			v = strings.TrimSpace(v)
			if !valid(v) {
				return nil, fmt.Errorf("跑腿标签 %s 含未知值 %q", field, v)
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return out, nil
	}
	var err error
	if d.Can, err = clean(d.Can, ValidDelegationTaskType, "can"); err != nil {
		return err
	}
	if d.Avoid, err = clean(d.Avoid, ValidDelegationRisk, "avoid"); err != nil {
		return err
	}
	d.Notes = strings.TrimSpace(d.Notes)
	if utf8.RuneCountInString(d.Notes) > 300 || strings.ContainsAny(d.Notes, "\x00\r") {
		return fmt.Errorf("跑腿备注最多 300 个字符")
	}
	if d.Enabled && len(d.Can) == 0 {
		return fmt.Errorf("启用跑腿时至少选择一种可接任务")
	}
	return nil
}

func cloneDelegation(d *ModelDelegation) *ModelDelegation {
	if d == nil {
		return nil
	}
	out := *d
	out.Can = slices.Clone(d.Can)
	out.Avoid = slices.Clone(d.Avoid)
	return &out
}

// DelegationTags returns the worker tags of an account model, or nil when the
// model is not offered as a worker.
func (a Account) DelegationTags(model string) *ModelDelegation {
	for _, m := range a.Models {
		if m.ID == model && m.Delegation != nil && m.Delegation.Enabled {
			return cloneDelegation(m.Delegation)
		}
	}
	return nil
}
