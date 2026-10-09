// Package modelcatalog holds official model metadata that needs no account:
// a snapshot compiled into the binary, and the catalogs the Claude Code and
// Codex CLIs carry, read offline from the configured Agent image. It is
// reference data for administrators; a relay serving the same model ID may
// still drop the reasoning parameter, so nothing here is applied at run time
// without being saved to an account.
package modelcatalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"agentbox/internal/config"
)

// Script runs inside a disposable sandbox from the Agent image; see cli.cjs.
//
//go:embed cli.cjs
var Script string

//go:embed builtin.json
var builtinRaw []byte

type Model struct {
	ID        string                      `json:"id"`
	Label     string                      `json:"label"`
	Reasoning *config.ReasoningCapability `json:"reasoning,omitempty"`
	// Current: offered by the CLI's own model picker; older entries stay
	// listed so relays still serving them get labels and reasoning.
	Current bool `json:"current,omitempty"`
}

type Snapshot struct {
	Schema     int                `json:"schema"`
	VerifiedAt string             `json:"verified_at"`
	Sources    map[string]string  `json:"sources"`
	Models     map[string][]Model `json:"models"`
}

var builtin = sync.OnceValues(func() (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(builtinRaw, &s); err != nil {
		return Snapshot{}, err
	}
	if s.Schema != 1 || s.VerifiedAt == "" {
		return Snapshot{}, errors.New("unsupported builtin catalog")
	}
	for agent, models := range s.Models {
		seen := map[string]bool{}
		for _, m := range models {
			if !config.ValidModelID(m.ID) || m.Label == "" || seen[m.ID] {
				return Snapshot{}, fmt.Errorf("builtin %s model %q invalid or repeated", agent, m.ID)
			}
			if err := config.ValidateReasoning(agent, m.Reasoning); err != nil {
				return Snapshot{}, fmt.Errorf("builtin %s model %s: %w", agent, m.ID, err)
			}
			seen[m.ID] = true
		}
	}
	return s, nil
})

// Builtin returns a copy of the compiled-in snapshot. Its validity is a test
// invariant; a broken file yields an empty catalog rather than a crash.
func Builtin() Snapshot {
	s, err := builtin()
	if err != nil {
		return Snapshot{}
	}
	out := s
	out.Models = make(map[string][]Model, len(s.Models))
	for agent, models := range s.Models {
		out.Models[agent] = Clone(models)
	}
	return out
}

func Clone(in []Model) []Model {
	out := slices.Clone(in)
	for i := range out {
		out[i].Reasoning = config.CloneReasoning(out[i].Reasoning)
	}
	return out
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// key drops what does not change the model: a -YYYYMMDD snapshot suffix
// (the alias names the latest snapshot) and Claude Code's [1m] context marker.
func key(id string) string {
	return dateSuffix.ReplaceAllString(strings.TrimSuffix(strings.ToLower(id), "[1m]"), "")
}

// Lookup finds the entry for a model ID: exact first, then by key.
func Lookup(models []Model, id string) (Model, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	k := key(id)
	for _, m := range models {
		if key(m.ID) == k {
			return m, true
		}
	}
	return Model{}, false
}

// Merge puts the CLI catalog first: it defines which models are current and
// its reasoning wins. Snapshot entries fill what it leaves out (labels,
// reasoning the CLI does not report, models it no longer lists).
func Merge(cli, snapshot []Model) []Model {
	out := Clone(cli)
	for i := range out {
		m := &out[i]
		b, ok := Lookup(snapshot, m.ID)
		if !ok {
			continue
		}
		if b.ID == m.ID {
			m.Label = b.Label
		}
		if m.Reasoning == nil {
			m.Reasoning = config.CloneReasoning(b.Reasoning)
		}
	}
	for _, b := range snapshot {
		if !slices.ContainsFunc(out, func(m Model) bool { return m.ID == b.ID }) {
			b.Current = false
			b.Reasoning = config.CloneReasoning(b.Reasoning)
			out = append(out, b)
		}
	}
	return out
}

type cliOutput struct {
	Version int `json:"version"`
	Claude  *struct {
		Error  string `json:"error"`
		Models []struct {
			Value          string   `json:"value"`
			ResolvedModel  string   `json:"resolvedModel"`
			DisplayName    string   `json:"displayName"`
			Description    string   `json:"description"`
			SupportsEffort bool     `json:"supportsEffort"`
			Levels         []string `json:"supportedEffortLevels"`
		} `json:"models"`
	} `json:"claude"`
	Codex *struct {
		Error  string `json:"error"`
		Models []struct {
			Model       string   `json:"model"`
			DisplayName string   `json:"displayName"`
			Hidden      bool     `json:"hidden"`
			Efforts     []string `json:"efforts"`
		} `json:"models"`
	} `json:"codex"`
}

// ParseCLI reads the driver's report. An agent whose CLI failed is absent
// from the result and named in failed; the other agent is still usable.
func ParseCLI(raw []byte) (models map[string][]Model, failed map[string]string, err error) {
	var out cliOutput
	if err := json.Unmarshal(raw, &out); err != nil || out.Version != 1 {
		return nil, nil, errors.New("CLI 模型目录输出无法识别")
	}
	models, failed = map[string][]Model{}, map[string]string{}
	add := func(agent string, m Model) {
		if config.ValidModelID(m.ID) && !slices.ContainsFunc(models[agent], func(x Model) bool { return x.ID == m.ID }) {
			models[agent] = append(models[agent], m)
		}
	}
	switch {
	case out.Claude == nil:
		failed[config.AgentClaude] = "missing"
	case out.Claude.Error != "" || len(out.Claude.Models) == 0:
		failed[config.AgentClaude] = firstNonEmpty(out.Claude.Error, "empty")
	default:
		for _, m := range out.Claude.Models {
			// "default" repeats another entry under the CLI's own alias.
			if m.Value == "default" {
				continue
			}
			// The 1M-context marker is a CLI option, not another model.
			id := strings.TrimSuffix(strings.TrimSpace(m.ResolvedModel), "[1m]")
			if !strings.HasPrefix(id, "claude-") {
				continue
			}
			entry := Model{ID: id, Label: claudeLabel(m.Description, m.DisplayName, id), Current: true}
			// A missing effort report is not proof of no reasoning control:
			// the snapshot decides (e.g. Haiku 4.5 takes a thinking budget).
			if levels := known(config.AgentClaude, "effort", m.Levels); m.SupportsEffort && len(levels) > 0 {
				entry.Reasoning = &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: levels}
			}
			add(config.AgentClaude, entry)
		}
	}
	switch {
	case out.Codex == nil:
		failed[config.AgentCodex] = "missing"
	case out.Codex.Error != "" || len(out.Codex.Models) == 0:
		failed[config.AgentCodex] = firstNonEmpty(out.Codex.Error, "empty")
	default:
		for _, m := range out.Codex.Models {
			// Hidden entries are internal (review, staging) models.
			if m.Hidden {
				continue
			}
			id := strings.TrimSpace(m.Model)
			entry := Model{ID: id, Label: firstNonEmpty(strings.TrimSpace(m.DisplayName), id), Current: true}
			if levels := known(config.AgentCodex, "effort", m.Efforts); len(levels) > 0 {
				entry.Reasoning = &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: levels}
			}
			add(config.AgentCodex, entry)
		}
	}
	for agent := range models {
		delete(failed, agent)
	}
	for _, agent := range []string{config.AgentClaude, config.AgentCodex} {
		if _, ok := models[agent]; !ok && failed[agent] == "" {
			failed[agent] = "empty"
		}
	}
	return models, failed, nil
}

// claudeLabel turns "Opus 5.5 with 1M context · Best for …" into
// "Claude Opus 5.5"; the picker's display name is only an alias ("Opus").
func claudeLabel(description, display, id string) string {
	name, _, _ := strings.Cut(description, " · ")
	name, _, _ = strings.Cut(name, " with ")
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(display)
	}
	if name == "" {
		return id
	}
	if !strings.HasPrefix(name, "Claude") {
		name = "Claude " + name
	}
	return name
}

func known(agent, control string, levels []string) []string {
	out := []string{}
	for _, l := range levels {
		if slices.Contains(config.ReasoningLevels(agent, control), l) && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
