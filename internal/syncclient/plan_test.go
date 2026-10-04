package syncclient

import (
	"errors"
	"fmt"
	"testing"

	"agentbox/internal/syncproto"
)

func planBinding() syncproto.Binding {
	return syncproto.Binding{Version: 1, Server: "https://fixture.invalid/", ServerID: syncproto.HashBytes([]byte("server")), User: "alice", Workspace: "space", Project: "project", ProjectPath: ".", LocalID: syncproto.HashBytes([]byte("disk:inode"))}
}
func tree(files map[string]string) syncproto.Manifest {
	rules, _ := syncproto.ParseRules("")
	m := syncproto.Manifest{Version: 1, RulesHash: rules.Hash(), Entries: map[string]syncproto.Entry{}}
	for name, content := range files {
		m.Entries[name] = syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte(content)), Size: int64(len(content))}
	}
	return m
}
func TestThreeWayPlanEditDeleteAndConvergedCases(t *testing.T) {
	for _, test := range []struct {
		name          string
		local, remote map[string]string
		kind          string
		conflict      bool
	}{
		{"upload", map[string]string{"a": "new"}, map[string]string{"a": "old"}, "upload", false},
		{"download", map[string]string{"a": "old"}, map[string]string{"a": "new"}, "download", false},
		{"delete-remote", map[string]string{}, map[string]string{"a": "old"}, "delete_remote", false},
		{"delete-local", map[string]string{"a": "old"}, map[string]string{}, "delete_local", false},
		{"delete-vs-edit", map[string]string{}, map[string]string{"a": "edited"}, "", true},
		{"both-edit", map[string]string{"a": "one"}, map[string]string{"a": "two"}, "", true},
		{"same-edit", map[string]string{"a": "same"}, map[string]string{"a": "same"}, "", false},
		{"both-delete", map[string]string{}, map[string]string{}, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := syncproto.Baseline{Binding: planBinding(), Local: tree(map[string]string{"a": "old"}), Remote: tree(map[string]string{"a": "old"})}
			plan, err := BuildPlan(base.Binding, &base, tree(test.local), tree(test.remote), PlanOptions{LocalExecutable: true})
			if err != nil {
				t.Fatal(err)
			}
			if (len(plan.Conflicts) > 0) != test.conflict {
				t.Fatalf("conflicts: %+v", plan)
			}
			if test.kind != "" {
				if len(plan.Operations) != 1 || plan.Operations[0].Kind != test.kind {
					t.Fatalf("operations: %+v", plan)
				}
			} else if len(plan.Operations) != 0 {
				t.Fatalf("unexpected writes: %+v", plan)
			}
			if test.conflict && plan.Ready(plan.Digest) == nil {
				t.Fatal("confirmation bypassed conflict")
			}
		})
	}
}

func TestInitialAndForceRequireExactPreviewConfirmation(t *testing.T) {
	l, r := tree(map[string]string{"a": "local"}), tree(map[string]string{"a": "remote"})
	plan, err := BuildPlan(planBinding(), nil, l, r, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 1 || plan.Ready(plan.Digest) == nil {
		t.Fatal("initial divergent content accepted")
	}
	plan, err = BuildPlan(planBinding(), nil, l, r, PlanOptions{Direction: PreferLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 1 || plan.Ready("") == nil || plan.Ready(plan.Digest) != nil {
		t.Fatalf("preview gate %+v", plan)
	}
	other, err := BuildPlan(planBinding(), nil, tree(map[string]string{"a": "changed-after-preview"}), r, PlanOptions{Direction: PreferLocal})
	if err != nil {
		t.Fatal(err)
	}
	if other.Ready(plan.Digest) == nil {
		t.Fatal("old confirmation accepted for changed tree")
	}
}

func TestFileChoicesCannotForceUnrelatedPathsOrStructuralConflicts(t *testing.T) {
	local, remote := tree(map[string]string{"a": "local", "only-local": "keep"}), tree(map[string]string{"a": "remote"})
	for _, options := range []PlanOptions{
		{Choices: map[string]Direction{"only-local": PreferRemote}},
		{Choices: map[string]Direction{"../outside": PreferLocal}},
		{Choices: map[string]Direction{"a": Automatic}},
		{Direction: PreferRemote, Choices: map[string]Direction{"a": PreferLocal}},
	} {
		if _, err := BuildPlan(planBinding(), nil, local, remote, options); !errors.Is(err, syncproto.ErrInvalid) {
			t.Fatal("invalid choice accepted", options, err)
		}
	}
	remote.Entries["a"] = syncproto.Entry{Kind: "directory"}
	if _, err := BuildPlan(planBinding(), nil, local, remote, PlanOptions{Choices: map[string]Direction{"a": PreferLocal}}); !errors.Is(err, syncproto.ErrInvalid) {
		t.Fatal("file/directory conflict forced", err)
	}
}

func TestBindingAndIgnoreChangesCannotBecomeDeletes(t *testing.T) {
	m := tree(map[string]string{"a": "x"})
	base := syncproto.Baseline{Binding: planBinding(), Local: m, Remote: m}
	changed := planBinding()
	changed.LocalID = "replacement-directory"
	if _, err := BuildPlan(changed, &base, m, m, PlanOptions{}); !errors.Is(err, ErrBinding) {
		t.Fatal(err)
	}
	n := tree(nil)
	rules, _ := syncproto.ParseRules("a")
	n.RulesHash = rules.Hash()
	if _, err := BuildPlan(base.Binding, &base, n, n, PlanOptions{}); !errors.Is(err, ErrRulesChanged) {
		t.Fatal("ignore change looked like deletion", err)
	}
}

func TestMassDeleteAndWindowsExecutablePreservation(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files[fmt.Sprintf("f%02d", i)] = "x"
	}
	remote := tree(files)
	base := syncproto.Baseline{Binding: planBinding(), Local: remote, Remote: remote}
	plan, err := BuildPlan(base.Binding, &base, tree(nil), remote, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NeedsConfirmation || plan.Ready("") == nil {
		t.Fatal("mass deletes unguarded")
	}
	l, r := tree(map[string]string{"run": "old"}), tree(map[string]string{"run": "old"})
	e := r.Entries["run"]
	e.Executable = true
	r.Entries["run"] = e
	base = syncproto.Baseline{Binding: planBinding(), Local: l, Remote: r}
	plan, err = BuildPlan(base.Binding, &base, tree(map[string]string{"run": "new"}), r, PlanOptions{LocalExecutable: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 1 || !plan.Operations[0].After.Executable {
		t.Fatal("Windows cleared remote executable bit", plan)
	}
}

func TestDirectoryDeletionDoesNotClobberNewRemoteChildren(t *testing.T) {
	l, r := tree(nil), tree(map[string]string{"dir/new": "remote-new"})
	r.Entries["dir"] = syncproto.Entry{Kind: "directory"}
	b := tree(nil)
	b.Entries["dir"] = syncproto.Entry{Kind: "directory"}
	base := syncproto.Baseline{Binding: planBinding(), Local: b, Remote: b}
	plan, err := BuildPlan(base.Binding, &base, l, r, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) == 0 || plan.Ready(plan.Digest) == nil {
		t.Fatal("directory deletion permitted with remote child", plan)
	}
}
