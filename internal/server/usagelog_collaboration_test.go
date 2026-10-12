package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
)

func TestUsageEventsAttributeCollaborationWithoutRebilling(t *testing.T) {
	s, source, handler := clientTestServer(t)
	u, _ := s.store.GetUser(source.User)
	if _, err := s.store.Update(source.ID, func(sess *store.Session) { sess.Name = "Original project" }); err != nil {
		t.Fatal(err)
	}
	roles := store.CollaborationRoles{
		Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "planner"},
		Executor:    store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "executor"},
	}
	input := store.CollaborationCreateInput{Scope: "usage-fixture", SourceSessionID: source.ID, Repository: ".", Baseline: strings.Repeat("a", 40), Text: "private task text", Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	run, _, err := s.store.CreateCollaboration(u, strings.Repeat("b", 32), input, store.CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.store.BeginCollaborationPreparation(u, run.ID, run.Revision)
	if err != nil {
		t.Fatal(err)
	}
	planner := store.Session{ID: "usage-planner", Name: "Internal participant", User: u.Name, Agent: "claude", AccountID: "shared", DefaultModel: "planner", Status: store.StatusStopped, CreatedAt: time.Now()}
	executor := planner
	executor.ID, executor.DefaultModel = "usage-executor", "executor"
	run, step, err := s.store.PublishCollaborationParticipants(u, run.ID, run.Revision, planner, executor, store.CollaborationStepInput{Text: "private step input"})
	if err != nil {
		t.Fatal(err)
	}
	base := store.UsageEvent{TS: time.Now(), User: u.Name, SessionID: planner.ID, ThreadID: run.ID, TurnID: step.TurnID, Kind: store.UsageKindChat, Agent: "claude", Model: "planner", InputTokens: 4, OutputTokens: 6, CostMicroUSD: 120, Price: &store.PriceSnapshot{Version: 1, Source: "table"}}
	child := base
	child.Model, child.CostMicroUSD = "child-model", 30
	ordinary := base
	ordinary.SessionID, ordinary.ThreadID, ordinary.TurnID, ordinary.Model = source.ID, "ordinary-thread", "ordinary-turn", "ordinary-model"
	if err = s.store.InsertUsage(base, child, ordinary); err != nil {
		t.Fatal(err)
	}
	before := s.store.ListUsage(store.UsageFilter{})
	ledger := s.store.ListLedger(store.LedgerFilter{})
	quota, _ := s.store.GetQuota(u.Name)
	// The actual authenticated route must include participant spend under the
	// original project and separate it from the ordinary-chat filter.
	for _, tc := range []struct {
		query string
		rows  int
		cost  int64
	}{
		{"?session=" + source.ID, 3, 270},
		{"?session=" + source.ID + "&kind=collaboration", 2, 150},
		{"?session=" + source.ID + "&kind=chat", 1, 120},
	} {
		w := clientRequest(handler, u.Name, "GET", "/api/usage/events"+tc.query, "")
		var result usageEventsResp
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if len(result.Rows) != tc.rows || result.Total.Rows != tc.rows || result.Total.CostMicroUSD != tc.cost {
			t.Fatal("inconsistent source or category filter", tc.query, result)
		}
		if strings.Contains(w.Body.String(), "private task text") || strings.Contains(w.Body.String(), "private step input") {
			t.Fatal("usage response leaked task content")
		}
		for _, row := range result.Rows {
			if row.TurnID == ordinary.TurnID {
				if row.Kind != "chat" || row.Collaboration != nil || row.SourceSessionName != "" {
					t.Fatal("ordinary row relabeled", row)
				}
				continue
			}
			want := store.UsageCollaboration{RunID: run.ID, StepID: step.ID, Role: "coordinator", StepKind: "plan", SourceSessionID: source.ID}
			if row.Kind != "collaboration" || row.Collaboration == nil || *row.Collaboration != want || row.SourceSessionName != "Original project" || row.SessionID != planner.ID || row.ThreadID != run.ID {
				t.Fatal("lost collaboration attribution or ledger identity", row)
			}
		}
	}
	page := getUsageEvents(t, s, "/api/usage/events?kind=collaboration&limit=1", "root", store.RoleAdmin)
	if len(page.Rows) != 1 || page.Total.Rows != 2 || page.Total.CostMicroUSD != 150 || page.Scope != "all" || len(page.Facet.Models) != 2 {
		t.Fatal("admin pagination lost matching totals or facets", page)
	}
	foreign := getUsageEvents(t, s, "/api/usage/events?kind=collaboration&user="+u.Name+"&session="+source.ID, "bob", store.RoleUser)
	if len(foreign.Rows) != 0 || foreign.Total.Rows != 0 || len(foreign.Facet.Models) != 0 || len(foreign.Facet.Users) != 0 {
		t.Fatal("foreign owner accessed collaboration usage", foreign)
	}
	afterQuota, _ := s.store.GetQuota(u.Name)
	if !reflect.DeepEqual(before, s.store.ListUsage(store.UsageFilter{})) || !reflect.DeepEqual(ledger, s.store.ListLedger(store.LedgerFilter{})) || !reflect.DeepEqual(quota, afterQuota) {
		t.Fatal("usage inspection changed settlement")
	}
}
