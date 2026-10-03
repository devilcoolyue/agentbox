package syncclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"agentbox/internal/syncproto"
)

var ErrBinding = errors.New("sync baseline belongs to another binding")
var ErrRulesChanged = errors.New("ignore rules changed; new preview and baseline confirmation required")

type Direction string

const (
	Automatic    Direction = "automatic"
	PreferLocal  Direction = "local"
	PreferRemote Direction = "remote"
)

type Operation struct {
	Kind   string           `json:"kind"` // upload, download, delete_local/remote, mkdir_local/remote, rmdir_local/remote.
	Path   string           `json:"path"`
	Before *syncproto.Entry `json:"before"` // nil explicitly requires absence.
	After  *syncproto.Entry `json:"after"`
}
type Conflict struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type Plan struct {
	Version           int               `json:"version"`
	Binding           syncproto.Binding `json:"binding"`
	LocalDigest       string            `json:"local_digest"`
	RemoteDigest      string            `json:"remote_digest"`
	Operations        []Operation       `json:"operations"`
	Conflicts         []Conflict        `json:"conflicts"`
	NeedsConfirmation bool              `json:"needs_confirmation"`
	Reasons           []string          `json:"reasons"`
	Digest            string            `json:"digest"`
}
type PlanOptions struct {
	Direction       Direction
	LocalExecutable bool
	// Choices applies only to actual file conflicts in the automatic plan.
	// It is persisted with the batch so reopening/reconciliation rebuilds it.
	Choices map[string]Direction `json:",omitempty"`
}

// BuildPlan is side-effect free. A project with any conflict must be paused as
// a whole. Applying is a separate operation requiring unchanged tree digests
// and (where requested) explicit confirmation of this exact preview digest.
func BuildPlan(binding syncproto.Binding, baseline *syncproto.Baseline, local, remote syncproto.Manifest, options PlanOptions) (Plan, error) {
	plan := Plan{Version: syncproto.Version, Binding: binding, Operations: []Operation{}, Conflicts: []Conflict{}, Reasons: []string{}}
	if binding.Validate() != nil {
		return plan, ErrBinding
	}
	if options.Direction == "" {
		options.Direction = Automatic
	}
	if options.Direction != Automatic && options.Direction != PreferLocal && options.Direction != PreferRemote {
		return plan, syncproto.ErrInvalid
	}
	if len(options.Choices) != 0 {
		if options.Direction != Automatic || len(options.Choices) > 1000 {
			return plan, syncproto.ErrInvalid
		}
		plain := options
		plain.Choices = nil
		base, err := BuildPlan(binding, baseline, local, remote, plain)
		if err != nil {
			return plan, err
		}
		allowed := map[string]bool{}
		for _, conflict := range base.Conflicts {
			if conflict.Reason == "both_sides_changed" || conflict.Reason == "initial_source_required" || conflict.Reason == "baseline_not_converged" {
				l, r := entry(local, conflict.Path), entry(remote, conflict.Path)
				if (l == nil || l.Kind == "file") && (r == nil || r.Kind == "file") {
					allowed[conflict.Path] = true
				}
			}
		}
		for name, choice := range options.Choices {
			if !allowed[name] || choice != PreferLocal && choice != PreferRemote {
				return plan, syncproto.ErrInvalid
			}
		}
	}
	var err error
	plan.LocalDigest, err = local.Digest()
	if err != nil {
		return plan, err
	}
	plan.RemoteDigest, err = remote.Digest()
	if err != nil {
		return plan, err
	}
	if local.RulesHash != remote.RulesHash {
		return plan, ErrRulesChanged
	}
	if baseline != nil {
		if baseline.Binding != binding {
			return plan, ErrBinding
		}
		if err = baseline.Local.Validate(); err != nil {
			return plan, err
		}
		if err = baseline.Remote.Validate(); err != nil {
			return plan, err
		}
		if baseline.Local.RulesHash != local.RulesHash || baseline.Remote.RulesHash != remote.RulesHash {
			return plan, ErrRulesChanged
		}
	}
	paths := map[string]bool{}
	for name := range local.Entries {
		paths[name] = true
	}
	for name := range remote.Entries {
		paths[name] = true
	}
	if baseline != nil {
		for name := range baseline.Local.Entries {
			paths[name] = true
		}
		for name := range baseline.Remote.Entries {
			paths[name] = true
		}
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		l, r := entry(local, name), entry(remote, name)
		if equivalent(l, r, options.LocalExecutable) {
			continue
		}
		if l != nil && r != nil && l.Kind != r.Kind {
			plan.Conflicts = append(plan.Conflicts, Conflict{name, "file_directory_type_change"})
			continue
		}
		if choice := options.Choices[name]; choice != "" {
			if choice == PreferLocal {
				plan.add(name, l, r, true, options.LocalExecutable)
			} else {
				plan.add(name, r, l, false, options.LocalExecutable)
			}
			continue
		}
		if options.Direction == PreferLocal {
			plan.add(name, l, r, true, options.LocalExecutable)
			continue
		}
		if options.Direction == PreferRemote {
			plan.add(name, r, l, false, options.LocalExecutable)
			continue
		}
		if baseline == nil {
			if l == nil {
				plan.add(name, r, l, false, options.LocalExecutable)
			} else if r == nil {
				plan.add(name, l, r, true, options.LocalExecutable)
			} else {
				plan.Conflicts = append(plan.Conflicts, Conflict{name, "initial_source_required"})
			}
			continue
		}
		beforeLocal, beforeRemote := entry(baseline.Local, name), entry(baseline.Remote, name)
		localChanged := !equivalent(l, beforeLocal, options.LocalExecutable)
		remoteChanged := !equivalent(r, beforeRemote, options.LocalExecutable)
		switch {
		case localChanged && remoteChanged:
			plan.Conflicts = append(plan.Conflicts, Conflict{name, "both_sides_changed"})
		case localChanged:
			plan.add(name, l, r, true, options.LocalExecutable)
		case remoteChanged:
			plan.add(name, r, l, false, options.LocalExecutable)
		default:
			plan.Conflicts = append(plan.Conflicts, Conflict{name, "baseline_not_converged"})
		}
	}
	// Deleting a directory while retaining any descendants would make partial
	// changes inevitable. Surface structural conflicts before the first write.
	removals := map[string]map[string]bool{"local": {}, "remote": {}}
	for _, operation := range plan.Operations {
		for _, side := range []string{"local", "remote"} {
			if operation.Kind == "delete_"+side || operation.Kind == "rmdir_"+side {
				removals[side][operation.Path] = true
			}
		}
	}
	structuralConflicts := map[string]bool{}
	for _, side := range []string{"local", "remote"} {
		target := local
		if side == "remote" {
			target = remote
		}
		for name := range target.Entries {
			if removals[side][name] {
				continue
			}
			for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
				if removals[side][parent] && !structuralConflicts[parent] {
					structuralConflicts[parent] = true
					plan.Conflicts = append(plan.Conflicts, Conflict{parent, "directory_contains_retained_changes"})
				}
			}
		}
	}
	sort.Slice(plan.Conflicts, func(i, j int) bool {
		if plan.Conflicts[i].Path != plan.Conflicts[j].Path {
			return plan.Conflicts[i].Path < plan.Conflicts[j].Path
		}
		return plan.Conflicts[i].Reason < plan.Conflicts[j].Reason
	})

	sort.Slice(plan.Operations, func(i, j int) bool {
		a, b := plan.Operations[i], plan.Operations[j]
		ar, br := operationRank(a.Kind), operationRank(b.Kind)
		if ar != br {
			return ar < br
		}
		if ar == 0 || ar == 3 {
			ad, bd := strings.Count(a.Path, "/"), strings.Count(b.Path, "/")
			if ad != bd {
				if ar == 3 {
					return ad > bd
				}
				return ad < bd
			}
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Kind < b.Kind
	})
	deletedLocal, deletedRemote := 0, 0
	for _, op := range plan.Operations {
		if op.Kind == "delete_local" {
			deletedLocal++
		}
		if op.Kind == "delete_remote" {
			deletedRemote++
		}
	}
	if baseline == nil {
		plan.confirm("initial_sync")
	}
	if options.Direction != Automatic {
		plan.confirm("force_" + string(options.Direction))
	}
	if len(options.Choices) != 0 {
		plan.confirm("file_choices")
	}
	if massDelete(deletedLocal, local) || massDelete(deletedRemote, remote) {
		plan.confirm("mass_delete")
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	plan.Digest = syncproto.HashBytes(encoded)
	return plan, nil
}

func (plan *Plan) confirm(reason string) {
	plan.NeedsConfirmation = true
	plan.Reasons = append(plan.Reasons, reason)
}
func entry(m syncproto.Manifest, name string) *syncproto.Entry {
	value, ok := m.Entries[name]
	if !ok {
		return nil
	}
	return &value
}
func equivalent(a, b *syncproto.Entry, executable bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.Hash == b.Hash && a.Size == b.Size && (!executable || a.Executable == b.Executable)
}
func (plan *Plan) add(name string, source, target *syncproto.Entry, toRemote, localExecutable bool) {
	side := "local"
	if toRemote {
		side = "remote"
	}
	op := Operation{Path: name, Before: target, After: source}
	switch {
	case source == nil:
		if target == nil {
			return
		}
		if target.Kind == "directory" {
			op.Kind = "rmdir_" + side
		} else {
			op.Kind = "delete_" + side
		}
	case source.Kind == "directory":
		op.Kind = "mkdir_" + side
	default:
		if toRemote {
			op.Kind = "upload"
		} else {
			op.Kind = "download"
		}
		if toRemote && !localExecutable && target != nil {
			copy := *source
			copy.Executable = target.Executable
			op.After = &copy
		}
	}
	plan.Operations = append(plan.Operations, op)
}
func operationRank(kind string) int {
	if strings.HasPrefix(kind, "mkdir_") {
		return 0
	}
	if strings.HasPrefix(kind, "delete_") {
		return 2
	}
	if strings.HasPrefix(kind, "rmdir_") {
		return 3
	}
	return 1
}
func massDelete(count int, m syncproto.Manifest) bool {
	files := 0
	for _, e := range m.Entries {
		if e.Kind == "file" {
			files++
		}
	}
	return count >= 20 || count >= 5 && count*4 >= files
}

func (plan Plan) Ready(confirmation string) error {
	if plan.Version != syncproto.Version || !syncproto.ValidHash(plan.Digest) {
		return syncproto.ErrInvalid
	}
	preview := plan
	preview.Digest = ""
	encoded, err := json.Marshal(preview)
	if err != nil {
		return err
	}
	if syncproto.HashBytes(encoded) != plan.Digest {
		return errors.New("sync preview was modified; rebuild it")
	}
	if len(plan.Conflicts) != 0 {
		return fmt.Errorf("sync paused: %d conflicts", len(plan.Conflicts))
	}
	if plan.NeedsConfirmation && confirmation != plan.Digest {
		return errors.New("explicit confirmation of the current sync preview is required")
	}
	return nil
}
