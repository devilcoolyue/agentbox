package delegation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
)

func worker(ref, tier string, can, avoid []string, in, out float64, priced bool) Worker {
	return Worker{Ref: ref, Model: ref + "-model", Tags: config.ModelDelegation{Enabled: true, Tier: tier, Can: can, Avoid: avoid}, PriceIn: in, PriceOut: out, Priced: priced}
}

func TestTaskNormalizeValidatesVocabulary(t *testing.T) {
	task := Task{Text: "  rename foo  ", Type: "mechanical_edit", Risks: []string{"public_api", "auth", "auth"}}
	if err := task.Normalize(); err != nil || task.Text != "rename foo" || task.Access != "write" || strings.Join(task.Risks, ",") != "auth,public_api" {
		t.Fatal(task, err)
	}
	for _, bad := range []Task{
		{Text: "", Type: "docs"},
		{Text: "x", Type: "design"},
		{Text: "x", Type: "docs", Risks: []string{"unknown"}},
		{Text: "x", Type: "docs", Access: "admin"},
		{Text: strings.Repeat("x", MaxTaskBytes+1), Type: "docs"},
	} {
		if err := bad.Normalize(); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	empty := Task{Text: "x", Type: "docs"}
	if err := empty.Normalize(); err != nil || empty.Risks == nil {
		t.Fatal("risks must serialize as an empty list", empty.Risks, err)
	}
}

func TestEligibleAppliesCanAndAvoidTags(t *testing.T) {
	sonnet := worker("w1", "economy", []string{"implement_spec", "write_tests"}, []string{"billing"}, 3, 15, true)
	codex := worker("w2", "economy", []string{"implement_spec"}, nil, 1, 4, true)
	off := worker("w3", "economy", []string{"implement_spec"}, nil, 0, 0, false)
	off.Tags.Enabled = false
	ok, rejected := Eligible([]Worker{sonnet, codex, off}, Task{Type: "implement_spec", Risks: []string{"billing"}})
	if len(ok) != 1 || ok[0].Ref != "w2" || rejected["w1"] != "risk:billing" || rejected["w3"] != "not_tagged" {
		t.Fatal(ok, rejected)
	}
	ok, rejected = Eligible([]Worker{sonnet}, Task{Type: "docs"})
	if len(ok) != 0 || rejected["w1"] != "task_type" {
		t.Fatal(ok, rejected)
	}
}

func TestPickPrefersEconomyThenKnownLowerPrice(t *testing.T) {
	standard := worker("w1", "standard", nil, nil, 0.1, 0.1, true)
	unpriced := worker("w2", "economy", nil, nil, 0, 0, false)
	cheap := worker("w3", "economy", nil, nil, 1, 4, true)
	pricey := worker("w4", "economy", nil, nil, 3, 15, true)
	if got := Pick([]Worker{standard, unpriced, pricey, cheap}); got.Ref != "w3" {
		t.Fatal(got.Ref)
	}
	if got := Pick([]Worker{standard, unpriced}); got.Ref != "w2" {
		t.Fatal("economy tier must win over a cheaper standard worker", got.Ref)
	}
	if got := Pick([]Worker{pricey, worker("w5", "economy", nil, nil, 3, 15, true)}); got.Ref != "w4" {
		t.Fatal("ties keep roster order", got.Ref)
	}
}

func TestDescriptionsCarryTagsNotBindings(t *testing.T) {
	w := worker("w1", "economy", []string{"docs"}, []string{"auth"}, 3, 15, true)
	w.AccountID, w.Tags.Notes = "secret-account", "批量改调用稳定"
	d := w.Description()
	if strings.Contains(d, "secret-account") || !strings.Contains(d, "docs") || !strings.Contains(d, "auth") || !strings.Contains(d, "批量改调用稳定") || !strings.Contains(d, "uncached input 3") {
		t.Fatal(d)
	}
	if s := JevState(Task{Text: "do it", Type: "docs", Access: "read"}); !strings.Contains(s, "Risks: none") || !strings.Contains(s, "do it") {
		t.Fatal(s)
	}
}

func TestPromptsNameToolsAndReadOnlyBoundary(t *testing.T) {
	main := MainPrompt([]Worker{worker("w1", "economy", []string{"docs"}, []string{"auth"}, 0, 0, false)})
	for _, want := range []string{DelegateTool, FeedbackTool, "declined", "mechanical_edit", "ambiguous_spec"} {
		if !strings.Contains(main, want) {
			t.Fatal("main prompt misses", want)
		}
	}
	if !strings.Contains(WorkerPrompt(Task{Access: "read"}), "do not modify any files") || strings.Contains(WorkerPrompt(Task{Access: "write"}), "do not modify any files") {
		t.Fatal("read-only instruction")
	}
	if in := WorkerInput(Task{Text: "fix it", Type: "fix_tests", Access: "write", Risks: []string{"auth"}}, false); !strings.Contains(in, "Known risks: auth") || !strings.HasSuffix(in, "fix it") {
		t.Fatal(in)
	}
}

func TestReportPrefersFinalAnswerAndTruncates(t *testing.T) {
	var r Report
	r.Observe([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`))
	if r.Text() != "working" {
		t.Fatal(r.Text())
	}
	r.Observe([]byte(`{"type":"result","subtype":"success","result":"## Done\nall good"}`))
	if r.Text() != "## Done\nall good" {
		t.Fatal(r.Text())
	}
	var codex Report
	codex.Observe([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`))
	codex.Observe([]byte(`{"type":"item.completed","item":{"type":"command_execution","text":"ls"}}`))
	codex.Observe([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"final"}}`))
	if codex.Text() != "final" {
		t.Fatal(codex.Text())
	}
	var long Report
	long.Observe([]byte(`{"type":"result","result":"` + strings.Repeat("中", MaxReportBytes) + `"}`))
	if len(long.Text()) > MaxReportBytes+4 || !strings.HasSuffix(long.Text(), "…") {
		t.Fatal(len(long.Text()))
	}
}

func TestManifestDiffSkipsBulkAndReportsChanges(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.go", "a")
	write("edit.go", "a")
	write("gone.go", "a")
	write("node_modules/x/index.js", "a")
	before := Snapshot(context.Background(), os.DirFS(dir))
	write("edit.go", "changed")
	past := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "edit.go"), past, past)
	write("new/file.go", "b")
	write("node_modules/x/index.js", "changed")
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	c := Diff(before, Snapshot(context.Background(), os.DirFS(dir)))
	if !c.Known || strings.Join(c.Added, ",") != "new/file.go" || strings.Join(c.Modified, ",") != "edit.go" || strings.Join(c.Deleted, ",") != "gone.go" || c.Total != 3 {
		t.Fatalf("%+v", c)
	}
	incomplete := Diff(Manifest{}, before)
	if incomplete.Known || len(incomplete.Added) != 0 {
		t.Fatal("an incomplete walk must not claim changes", incomplete)
	}
}
