package store

import (
	"reflect"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
)

func usageEventIDs(events []UsageEvent) []int64 {
	ids := make([]int64, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}
	return ids
}

func TestUsageCollaborationFiltersShareSourceTotalsFacetsAndPagination(t *testing.T) {
	s, u, run := preparedCollaboration(t, true)
	if _, err := s.Grant(u.Name, 1000, "usage-collaboration-test", "", "admin"); err != nil {
		t.Fatal(err)
	}
	plan := activeCollaborationStep(t, s, run.ID)
	base := UsageEvent{TS: time.Now(), User: u.Name, SessionID: "planner", ThreadID: run.ID, TurnID: plan.TurnID, Agent: "codex", Model: "planner", Kind: UsageKindChat, InputTokens: 10, CostMicroUSD: 10}
	extra := base
	extra.Model, extra.InputTokens, extra.CostMicroUSD = "helper", 20, 20
	run = finishCollaborationFixture(t, s, u, run, "plan", collaboration.Plan{Summary: "Plan", Items: []collaboration.PlanItem{{ID: "one", Text: "Implement", Acceptance: []string{"test"}}}})
	execute := activeCollaborationStep(t, s, run.ID)
	worker := base
	worker.SessionID, worker.TurnID, worker.Agent, worker.Model, worker.InputTokens, worker.CostMicroUSD = "builder", execute.TurnID, "claude", "builder", 30, 30
	direct := base
	direct.SessionID, direct.ThreadID, direct.TurnID, direct.Model, direct.InputTokens, direct.CostMicroUSD = "source", "thread", "direct", "direct", 40, 40
	title := base
	title.Kind, title.Model, title.InputTokens, title.CostMicroUSD = UsageKindTitle, "title", 50, 50
	foreign := base
	foreign.User, foreign.Model = "bob", "foreign"
	if err := s.InsertUsage(base, extra, worker, direct, title, foreign); err != nil {
		t.Fatal(err)
	}
	beforeRows := s.ListUsage(UsageFilter{})
	beforeLedger := s.ListLedger(LedgerFilter{})
	beforeQuota, _ := s.GetQuota(u.Name)
	if rows := s.ListUsage(UsageFilter{User: u.Name, SessionID: "source"}); len(rows) != 1 || rows[0].TurnID != "direct" {
		t.Fatal("default session query changed", rows)
	}
	filter := UsageFilter{User: u.Name, SessionID: "source", IncludeCollaborationSessions: true}
	if total := s.SumUsage(filter); total.Rows != 4 || total.Turns != 3 || total.InputTokens != 100 || total.CostMicroUSD != 100 {
		t.Fatal("source totals did not include both participants once", total)
	}
	filter.Collaboration, filter.Limit, filter.Asc = "only", 1, true
	if total := s.SumUsage(filter); total.Rows != 3 || total.Turns != 2 || total.InputTokens != 60 || total.CostMicroUSD != 60 {
		t.Fatal("model-step totals disagree with source filter", total)
	}
	var paged []UsageEvent
	for offset := 0; offset < 4; offset++ {
		filter.Offset = offset
		paged = append(paged, s.ListUsage(filter)...)
	}
	if len(paged) != 3 || paged[0].Model != "planner" || paged[1].Model != "helper" || paged[2].Model != "builder" {
		t.Fatal("model-step pagination duplicated or omitted rows", paged)
	}
	filter.Agent, filter.Model = "codex", "planner"
	facets := s.FacetsUsage(filter, u.Name)
	if !reflect.DeepEqual(facets, UsageFacets{Users: []string{u.Name}, Agents: []string{"codex"}, Models: []string{"helper", "planner"}}) {
		t.Fatal("facets lost collaboration or visibility filters", facets)
	}
	filter.Agent, filter.Model, filter.Collaboration = "", "", "exclude"
	if total := s.SumUsage(filter); total.Rows != 1 || total.CostMicroUSD != 40 {
		t.Fatal("ordinary chat did not remain directly visible", total)
	}
	filter.SessionID = ""
	if total := s.SumUsage(filter); total.Rows != 2 || total.CostMicroUSD != 90 {
		t.Fatal("exclude removed auxiliary usage", total)
	}
	if total := s.SumUsage(UsageFilter{User: u.Name, SessionID: "planner", IncludeCollaborationSessions: true, Collaboration: "only"}); total.Rows != 2 {
		t.Fatal("participant's own session filter stopped matching", total)
	}
	attribution, err := s.UsageCollaborations(usageEventIDs(beforeRows))
	if err != nil || len(attribution) != 3 {
		t.Fatal(attribution, err)
	}
	for _, event := range paged {
		want := UsageCollaboration{RunID: run.ID, StepID: plan.ID, Role: "coordinator", StepKind: "plan", SourceSessionID: "source"}
		if event.SessionID == "builder" {
			want.StepID, want.Role, want.StepKind = execute.ID, "executor", "execute"
		}
		if attribution[event.ID] != want {
			t.Fatal("wrong original step attribution", event, attribution[event.ID])
		}
	}
	if !reflect.DeepEqual(beforeRows, s.ListUsage(UsageFilter{})) || !reflect.DeepEqual(beforeLedger, s.ListLedger(LedgerFilter{})) {
		t.Fatal("usage inspection mutated accounting")
	}
	afterQuota, _ := s.GetQuota(u.Name)
	if beforeQuota != afterQuota {
		t.Fatal("usage inspection changed credit", beforeQuota, afterQuota)
	}
}

func TestUsageCollaborationRejectsMismatchedIdentityAndEmptyTurns(t *testing.T) {
	s, u, run := preparedCollaboration(t, false)
	step := activeCollaborationStep(t, s, run.ID)
	base := UsageEvent{User: u.Name, SessionID: "planner", ThreadID: run.ID, TurnID: step.TurnID, Agent: "codex", Kind: UsageKindChat}
	for _, mutate := range []func(*UsageEvent){
		func(e *UsageEvent) { e.User = "bob" },
		func(e *UsageEvent) { e.ThreadID = "foreign-run" },
		func(e *UsageEvent) { e.SessionID = "builder" },
		func(e *UsageEvent) { e.TurnID = "foreign-turn" },
		func(e *UsageEvent) { e.TurnID = "" },
		func(e *UsageEvent) { e.Kind = UsageKindTitle },
		func(e *UsageEvent) { e.Kind = UsageKindTerminal },
		func(e *UsageEvent) { e.Kind = UsageKindDecision },
	} {
		foreign := base
		mutate(&foreign)
		if err := s.InsertUsage(foreign); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertUsage(base); err != nil {
		t.Fatal(err)
	}
	rows := s.ListUsage(UsageFilter{Collaboration: "only"})
	if len(rows) != 1 || rows[0].User != u.Name || rows[0].SessionID != "planner" {
		t.Fatal("foreign accounting matched a task", rows)
	}
	all := s.ListUsage(UsageFilter{})
	attribution, err := s.UsageCollaborations(usageEventIDs(all))
	if err != nil || len(attribution) != 1 || attribution[rows[0].ID].StepID != step.ID {
		t.Fatal("foreign accounting received task metadata", attribution, err)
	}
	// Empty turn IDs are allowed for undispatched steps; they identify no turn.
	if _, err = s.db.Exec("UPDATE collaboration_steps SET turn_id='' WHERE id=?", step.ID); err != nil {
		t.Fatal(err)
	}
	attribution, err = s.UsageCollaborations(usageEventIDs(all))
	if err != nil || len(attribution) != 0 || s.SumUsage(UsageFilter{Collaboration: "only"}).Rows != 0 {
		t.Fatal("empty turn IDs matched", attribution, err)
	}
}

func TestUsageCollaborationSurvivesParticipantReplacementAndDeletedTask(t *testing.T) {
	s, u, run := preparedCollaboration(t, false)
	step := activeCollaborationStep(t, s, run.ID)
	if err := s.InsertUsage(UsageEvent{User: u.Name, SessionID: "planner", ThreadID: run.ID, TurnID: step.TurnID, Agent: "codex", Kind: UsageKindChat}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO collaboration_participants(run_id,role,generation,config_version,session_id,created_at)
		VALUES(?,'coordinator',2,1,'new-planner',?)`, run.ID, usageTimestamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE collaboration_participants SET state='revoked' WHERE session_id='planner'"); err != nil {
		t.Fatal(err)
	}
	run = applyCollaborationFixture(t, s, u, run, "pause", nil)
	if err := s.DeleteCollaboration(u, run.ID, run.Scope, run.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM sessions WHERE id IN ('source','planner','builder')"); err != nil {
		t.Fatal(err)
	}
	rows := s.ListUsage(UsageFilter{User: u.Name, SessionID: "source", IncludeCollaborationSessions: true, Collaboration: "only"})
	if len(rows) != 1 || rows[0].SessionID != "planner" {
		t.Fatal("historical participant or deleted source disappeared", rows)
	}
	// Exercise multi-batch attribution without manufacturing hundreds of rows.
	ids := make([]int64, 805)
	for i := range ids {
		ids[i] = rows[0].ID
	}
	attribution, err := s.UsageCollaborations(ids)
	if err != nil || len(attribution) != 1 || attribution[rows[0].ID].StepID != step.ID {
		t.Fatal("tombstone attribution lost original step", attribution, err)
	}
	if empty, err := s.UsageCollaborations(nil); err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
}

func TestUsageCollaborationDecisionAttributionKeepsSeparateKind(t *testing.T) {
	s, u, in, cfg, snapshot := decisionFixture(t)
	run, record := acceptDecision(t, s, u, in, cfg, snapshot)
	attempt, _, err := s.BeginCollaborationDecisionAttempt(u, run.ID, record.Revision, NewCollaborationID(), "mindshub", config.DecisionJevModel, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, run.ID, attempt.ID, decision.Usage{Known: true, InputTokens: 10, OutputTokens: 1, CostStatus: "known"}, config.DecisionJevModel); err != nil {
		t.Fatal(err)
	}
	rows := s.ListUsage(UsageFilter{Kind: UsageKindDecision})
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	ids := usageEventIDs(rows)
	attribution, err := s.UsageCollaborations(ids)
	want := UsageCollaboration{RunID: run.ID, SourceSessionID: "source"}
	if err != nil || len(attribution) != 1 || attribution[rows[0].ID] != want {
		t.Fatal(attribution, err)
	}
	if s.SumUsage(UsageFilter{Collaboration: "only"}).Rows != 0 || s.SumUsage(UsageFilter{Collaboration: "exclude"}).Rows != 1 {
		t.Fatal("decision usage was classified as a model step")
	}
	for _, field := range []string{"user", "session_id", "thread_id", "turn_id", "req_id"} {
		t.Run(field, func(t *testing.T) {
			var previous string
			if err := s.db.QueryRow("SELECT "+field+" FROM usage_events WHERE id=?", ids[0]).Scan(&previous); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE usage_events SET "+field+"='foreign' WHERE id=?", ids[0]); err != nil {
				t.Fatal(err)
			}
			got, err := s.UsageCollaborations(ids)
			if err != nil || len(got) != 0 {
				t.Fatal("foreign decision attributed to task", got, err)
			}
			if _, err := s.db.Exec("UPDATE usage_events SET "+field+"=? WHERE id=?", previous, ids[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
}
