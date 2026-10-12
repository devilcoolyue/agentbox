package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
)

func decisionFixture(t *testing.T) (*Store, User, CollaborationCreateInput, CollaborationConfig, decision.Snapshot) {
	t.Helper()
	s, u, in, cfg := collaborationFixture(t)
	in.Decision = &decision.Intent{Version: 1, Provider: "jev", Roles: map[collaboration.Role]decision.RoleIntent{collaboration.Coordinator: {Mode: "fixed"}, collaboration.Executor: {Mode: "auto"}}}
	cheap := in.Roles.Executor
	cheap.Model = "economy"
	snap := decision.Snapshot{Version: 1, ConfigRevision: 1, Provider: "jev", Roles: map[collaboration.Role]decision.RoleSnapshot{
		collaboration.Coordinator: {Mode: "fixed", Binding: in.Roles.Coordinator},
		collaboration.Executor:    {Mode: "auto", Fallback: in.Roles.Executor, Candidates: []decision.Candidate{{Ref: "standard", Binding: in.Roles.Executor}, {Ref: "economy", Binding: cheap}}},
	}}
	return s, u, in, cfg, snap
}

func fallbackResult(s decision.Snapshot) decision.Result {
	out := decision.Result{Decisions: map[collaboration.Role]decision.Decision{}}
	for role, r := range s.Roles {
		d := decision.Decision{Binding: r.Binding, Source: "manual", Reason: "fixed"}
		if r.Mode == "auto" {
			d.Binding = r.Fallback
			d.Source = "fallback"
			d.Reason = "unavailable"
		}
		out.Decisions[role] = d
		if role == collaboration.Coordinator {
			out.Roles.Coordinator = d.Binding
		} else {
			out.Roles.Executor = d.Binding
		}
	}
	return out
}

func acceptDecision(t *testing.T, s *Store, u User, in CollaborationCreateInput, cfg CollaborationConfig, snap decision.Snapshot) (CollaborationRun, CollaborationDecisionRecord) {
	t.Helper()
	r, fresh, err := s.CreateCollaborationWithDecision(u, NewCollaborationID(), in, cfg, snap)
	if err != nil || !fresh {
		t.Fatal(r, fresh, err)
	}
	d, err := s.CollaborationDecision(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, fresh, err = s.ClaimCollaborationDecision(u, r.ID, d.Revision)
	if err != nil || !fresh {
		t.Fatal(d, fresh, err)
	}
	return r, d
}

func TestDecisionAcceptanceSnapshotAndImmutableRequest(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	id := NewCollaborationID()
	if _, _, err := s.CreateCollaboration(u, id, in, cfg); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("automatic input without snapshot accepted", err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER fail_snapshot BEFORE INSERT ON collaboration_decisions BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateCollaborationWithDecision(u, id, in, cfg, snap); err == nil {
		t.Fatal("accepted without durable snapshot")
	}
	if _, err := s.CollaborationByRequest(u, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("partial acceptance", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_snapshot"); err != nil {
		t.Fatal(err)
	}
	r, fresh, err := s.CreateCollaborationWithDecision(u, id, in, cfg, snap)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	if _, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision); !errors.Is(err, collaboration.ErrState) {
		t.Fatal("preparation before binding freeze", err)
	}
	cfgRaw, _ := json.Marshal(cfg)
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "configure", Payload: cfgRaw}); !errors.Is(err, collaboration.ErrState) {
		t.Fatal("configure bypassed routing", err)
	}
	d, claimed, err := s.ClaimCollaborationDecision(u, r.ID, 1)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	result := fallbackResult(snap)
	cheap := snap.Roles[collaboration.Executor].Candidates[1].Binding
	result.Roles.Executor = cheap
	result.Decisions[collaboration.Executor] = decision.Decision{Binding: cheap, Source: "builtin", Reason: "local_edit"}
	r, err = s.FreezeCollaborationDecision(u, r.ID, d.Revision, result)
	if err != nil {
		t.Fatal(err)
	}
	if r.Request.Roles != in.Roles {
		t.Fatal("changed request/fingerprint")
	}
	chosen, err := s.CollaborationConfig(r.ID, 1)
	if err != nil || chosen.Roles.Executor != cheap {
		t.Fatal(chosen, err)
	}
	// Both HTTP replay and changed mutable server defaults preserve the initial snapshot.
	if _, fresh, err = s.CreateCollaboration(u, id, in, cfg); err != nil || fresh {
		t.Fatal("replay required a new policy snapshot", fresh, err)
	}
	snap.ConfigRevision = 99
	if _, fresh, err = s.CreateCollaborationWithDecision(u, id, in, cfg, snap); err != nil || fresh {
		t.Fatal(fresh, err)
	}
	got, err := s.CollaborationDecision(u, r.ID)
	if err != nil || got.Snapshot.ConfigRevision != 1 {
		t.Fatal(got, err)
	}
	if _, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestMindshubDecisionFreeReservationAndDistinctPrice(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	if _, err := s.Grant(u.Name, 10000, "mindshub-fixture-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	for _, v := range []struct {
		provider, model string
		bound           int64
	}{{"jev", config.DecisionJevModel, 0}, {"unknown", config.DecisionJevModel, 0}, {"mindshub", "jev-latest", 0}, {"mindshub", config.DecisionJevModel, 2753}} {
		if _, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), v.provider, v.model, v.bound); !errors.Is(err, ErrChatInvalid) {
			t.Fatalf("unproved zero/free reservation accepted %+v %v", v, err)
		}
	}
	a, fresh, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "mindshub", config.DecisionJevModel, 0)
	if err != nil || !fresh || a.Provider != "mindshub" || a.ReservedMicroUSD != 0 {
		t.Fatal(a, fresh, err)
	}
	if err := s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, decision.Usage{CostStatus: "unknown"}, ""); err != nil {
		t.Fatal(err)
	}
	cost, err := s.CollaborationCost(u, r.ID)
	if err != nil || !cost.DecisionPartial || cost.DecisionReservedMicroUSD != 0 || cost.DecisionCalls != 1 {
		t.Fatal(cost, err)
	}
	wrong := decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, OutputTokens: 30, CostMicroUSD: 42}
	if err := s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, wrong, config.DecisionJevModel); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("TypeSafe price leaked into MindsHub", err)
	}
	known := wrong
	known.CostMicroUSD = 0
	for i := 0; i < 2; i++ {
		if err := s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, known, config.DecisionJevModel); err != nil {
			t.Fatal(err)
		}
	}
	rows := s.ListUsage(UsageFilter{User: u.Name, Kind: UsageKindDecision})
	if len(rows) != 1 || rows[0].Provider != "mindshub" || rows[0].InputTokens != 1000 || rows[0].OutputTokens != 30 || rows[0].CostMicroUSD != 0 || rows[0].Price == nil || rows[0].Price.Source != "table" || rows[0].Price.Standard.Input != 0 || rows[0].Price.PricingRevision != decision.MindshubJevPricingVersion || rows[0].Price.SourceURL != decision.MindshubJevPricingSource {
		t.Fatalf("bad free usage evidence %+v", rows)
	}
	q, _ := s.GetQuota(u.Name)
	if q.BalanceMicroUSD != 10000 {
		t.Fatal("free gateway debited", q)
	}
	cost, err = s.CollaborationCost(u, r.ID)
	if err != nil || cost.DecisionPartial || cost.DecisionCalls != 1 || cost.DecisionCostMicroUSD != 0 {
		t.Fatal(cost, err)
	}
}

func TestDecisionClaimDispatchAndLedgerExactlyOnce(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	var path string
	if err := s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	connections := []*Store{s, other}
	if _, err := s.Grant(u.Name, 10_000, "decision-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	r, fresh, err := s.CreateCollaborationWithDecision(u, NewCollaborationID(), in, cfg, snap)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	var grants atomic.Int32
	var wg sync.WaitGroup
	for i := range 12 {
		connection := connections[i%len(connections)]
		wg.Go(func() {
			_, fresh, err := connection.ClaimCollaborationDecision(u, r.ID, 1)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				grants.Add(1)
			}
		})
	}
	wg.Wait()
	if grants.Load() != 1 {
		t.Fatal("routing executed repeatedly", grants.Load())
	}
	d, err := s.CollaborationDecision(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	grants.Store(0)
	for i := range 12 {
		connection := connections[i%len(connections)]
		wg.Go(func() {
			_, fresh, err := connection.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				grants.Add(1)
			}
		})
	}
	wg.Wait()
	if grants.Load() != 1 {
		t.Fatal("provider dispatched repeatedly", grants.Load())
	}
	d, err = s.CollaborationDecision(u, r.ID)
	if err != nil || d.Attempt == nil {
		t.Fatal(err)
	}
	if reserved, err := s.DecisionReservedMicroUSD(u.Name); err != nil || reserved != 1000 {
		t.Fatal(reserved, err)
	}
	unknown := decision.Usage{CostStatus: "unknown"}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, d.Attempt.ID, unknown, ""); err != nil {
		t.Fatal(err)
	}
	cost, err := s.CollaborationCost(u, r.ID)
	if err != nil || cost.DecisionReservedMicroUSD != 1000 || !cost.DecisionPartial || cost.Partial || cost.CostMicroUSD != 0 || cost.DecisionCalls != 1 {
		t.Fatal(cost, err)
	}
	known := decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, CostMicroUSD: 42}
	for i := range 12 {
		connection := connections[i%len(connections)]
		wg.Go(func() {
			if err := connection.SettleCollaborationDecisionAttempt(u, r.ID, d.Attempt.ID, known, "jev-1.13.0"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if q, _ := s.GetQuota(u.Name); q.BalanceMicroUSD != 9958 {
		t.Fatal("duplicate ledger debit", q)
	}
	rows := s.ListUsage(UsageFilter{User: u.Name, Kind: UsageKindDecision})
	if len(rows) != 1 || rows[0].Agent != "decision" || rows[0].CostMicroUSD != 42 {
		t.Fatal(rows)
	}
	if p := rows[0].Price; p == nil || p.Source != "table" || p.Standard.Input != decision.JevInputUSDPerMillion || p.PricingRevision != decision.JevPricingVersion {
		t.Fatal("decision price source misattributed", rows[0].Price)
	}
	cost, err = s.CollaborationCost(u, r.ID)
	if err != nil || cost.DecisionReservedMicroUSD != 0 || cost.DecisionPartial || cost.DecisionCostMicroUSD != 42 || cost.CostMicroUSD != 42 {
		t.Fatal(cost, err)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, d.Attempt.ID, unknown, ""); err != nil {
		t.Fatal(err)
	}
	known.CostMicroUSD = 99
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, d.Attempt.ID, known, "jev-1.13.0"); !errors.Is(err, ErrChatConflict) {
		t.Fatal("settled evidence overwritten", err)
	}
}

func TestDecisionReservationBudgetAndSettlementRollback(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	if _, err := s.Grant(u.Name, 1500, "decision-budget", "", "admin"); err != nil {
		t.Fatal(err)
	}
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	a, fresh, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	r2, d2 := acceptDecision(t, s, u, in, cfg, snap)
	if _, _, err = s.BeginCollaborationDecisionAttempt(u, r2.ID, d2.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000); !errors.Is(err, decision.ErrBudgetUnknown) {
		t.Fatal("reserved credit reused by another task", err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER fail_decision_debit BEFORE INSERT ON credit_ledger WHEN NEW.reason='spend' BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	usage := decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, CostMicroUSD: 42}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, usage, "jev-1.13.0"); err == nil {
		t.Fatal("ledger failure ignored")
	}
	if got := s.ListUsage(UsageFilter{User: u.Name, Kind: UsageKindDecision}); len(got) != 0 {
		t.Fatal("partial usage survived", got)
	}
	if reserved, err := s.DecisionReservedMicroUSD(u.Name); err != nil || reserved != 1000 {
		t.Fatal(reserved, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_decision_debit"); err != nil {
		t.Fatal(err)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, decision.Usage{CostStatus: "not_dispatched"}, ""); err != nil {
		t.Fatal(err)
	}
	if reserved, err := s.DecisionReservedMicroUSD(u.Name); err != nil || reserved != 0 {
		t.Fatal(reserved, err)
	}
	in.BudgetMicroUSD = 500
	cfg.BudgetMicroUSD = 500
	r3, d3 := acceptDecision(t, s, u, in, cfg, snap)
	if _, _, err = s.BeginCollaborationDecisionAttempt(u, r3.ID, d3.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 501); !errors.Is(err, decision.ErrBudgetUnknown) {
		t.Fatal("task reservation exceeded budget", err)
	}
}

func TestDecisionRecoveryCancellationAndDeletionFences(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	a, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.CollaborationDecision(u, r.ID)
	if err != nil || recovered.State != "interrupted" || recovered.Attempt.State != "unknown" {
		t.Fatal(recovered, err)
	}
	if _, fresh, err := s.ClaimCollaborationDecision(u, r.ID, recovered.Revision); err != nil || fresh {
		t.Fatal("restart granted reroute", fresh, err)
	}
	if _, err = s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap)); !errors.Is(err, ErrChatConflict) {
		t.Fatal("late decision survived recovery", err)
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "resume"}); err != nil {
		t.Fatal(err)
	}
	result := fallbackResult(snap)
	r, err = s.FreezeCollaborationDecision(u, r.ID, recovered.Revision, result)
	if err != nil {
		t.Fatal("explicit resumed fallback failed", err)
	}
	if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	r, err = s.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.FinalizeCollaborationCancellation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteCollaboration(u, r.ID, r.Scope, r.Revision); err != nil {
		t.Fatal(err)
	}
	redacted, err := s.CollaborationDecision(u, r.ID)
	if err != nil || redacted.Snapshot != nil || redacted.Result != nil || redacted.State != "deleted" {
		t.Fatal(redacted, err)
	}
	if reserved, err := s.DecisionReservedMicroUSD(u.Name); err != nil || reserved != 1000 {
		t.Fatal("deletion erased financial reservation", reserved, err)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, CostMicroUSD: 42}, "jev-1.13.0"); err != nil {
		t.Fatal("late fee after deletion", err)
	}
	redacted, _ = s.CollaborationDecision(u, r.ID)
	if redacted.State != "deleted" || redacted.Result != nil {
		t.Fatal("fee revived task", redacted)
	}
}

func TestDecisionUserEpochAndDefaultsCleanup(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	if _, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCollaborationDecisionDefault(u, "source", "jev"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCollaborationDecisionDefault(u, "source"); err != nil || got != "jev" {
		t.Fatal(got, err)
	}
	if err := s.DeleteUser(u.Name); err != nil {
		t.Fatal(err)
	}
	u.CreatedAt = u.CreatedAt.Add(time.Hour)
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if reserved, err := s.DecisionReservedMicroUSD(u.Name); err != nil || reserved != 0 {
		t.Fatal("new identity inherited reservation", reserved, err)
	}
	if got, err := s.GetCollaborationDecisionDefault(u, "source"); err != nil || got != "inherit" {
		t.Fatal("new identity inherited default", got, err)
	}
	if err := s.SetCollaborationDecisionDefault(u, "source", "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("source"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM collaboration_decision_defaults").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestDecisionMigration15PreservesExistingRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(db, migrations()[:14]); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db}
	u := User{Name: "alice", CreatedAt: time.Now().UTC(), Role: RoleUser}
	if err = old.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if err = old.Put(Session{ID: "source", User: u.Name}); err != nil {
		t.Fatal(err)
	}
	roles := CollaborationRoles{Coordinator: CollaborationBinding{AgentType: "claude", AccountID: "a", Model: "planner"}, Executor: CollaborationBinding{AgentType: "codex", AccountID: "b", Model: "worker"}}
	in := CollaborationCreateInput{Scope: "scope", SourceSessionID: "source", Repository: ".", Baseline: strings.Repeat("a", 40), Text: "legacy", Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	cfg := CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	r, _, err := old.CreateCollaboration(u, NewCollaborationID(), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err = old.db.QueryRow("SELECT fingerprint FROM collaborations WHERE id=?", r.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var after string
	if err = s.db.QueryRow("SELECT fingerprint FROM collaborations WHERE id=?", r.ID).Scan(&after); err != nil || before != after {
		t.Fatal("migration changed request fingerprint", err)
	}
	if _, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision); err != nil {
		t.Fatal("legacy task gated on missing decision", err)
	}
}

func TestDecisionBudgetStopsPreparationAndCoreFailureStopsResume(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	in.BudgetMicroUSD = 1000
	cfg.BudgetMicroUSD = 1000
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	a, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap)); !errors.Is(err, collaboration.ErrState) {
		t.Fatal("unresolved external attempt bypassed durable evidence", err)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, decision.Usage{CostStatus: "unknown"}, ""); err != nil {
		t.Fatal(err)
	}
	r, err = s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := s.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if !errors.Is(err, collaboration.ErrState) || stopped.State != CollabPaused || stopped.WaitReason != "budget_limit" {
		t.Fatal(stopped, err)
	}
	// A settlement write failure is not plugin unavailability. Explicit resume
	// is forbidden until the original attempt's fee evidence has been committed.
	in.BudgetMicroUSD = 0
	cfg.BudgetMicroUSD = 0
	r2, d2 := acceptDecision(t, s, u, in, cfg, snap)
	a2, _, err := s.BeginCollaborationDecisionAttempt(u, r2.ID, d2.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FailCollaborationDecision(u, r2.ID, d2.Revision, "decision_persistence_failed"); err != nil {
		t.Fatal(err)
	}
	r2, _ = s.Collaboration(u, r2.ID)
	if _, _, err = s.ApplyCollaborationAction(u, r2.ID, NewCollaborationID(), CollaborationActionInput{Scope: r2.Scope, ExpectedRevision: r2.Revision, Action: "resume"}); !errors.Is(err, collaboration.ErrState) {
		t.Fatal("core accounting failure resumed", err)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, r2.ID, a2.ID, decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, CostMicroUSD: 42}, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	r2, _ = s.Collaboration(u, r2.ID)
	if r2.State != CollabPaused || r2.WaitReason != "decision_failed" {
		t.Fatal("settlement executed task", r2)
	}
	if _, _, err = s.ApplyCollaborationAction(u, r2.ID, NewCollaborationID(), CollaborationActionInput{Scope: r2.Scope, ExpectedRevision: r2.Revision, Action: "resume"}); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionCannotChangeFixedOrOutOfPoolBinding(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	for _, mutate := range []func(*decision.Result){
		func(v *decision.Result) {
			v.Roles.Coordinator.Model = "other"
			c := v.Decisions[collaboration.Coordinator]
			c.Binding = v.Roles.Coordinator
			v.Decisions[collaboration.Coordinator] = c
		},
		func(v *decision.Result) {
			v.Roles.Executor.Model = "out-of-pool"
			c := v.Decisions[collaboration.Executor]
			c.Binding = v.Roles.Executor
			v.Decisions[collaboration.Executor] = c
		},
		func(v *decision.Result) {
			c := v.Decisions[collaboration.Executor]
			c.Source = "jev"
			v.Decisions[collaboration.Executor] = c
		},
	} {
		v := fallbackResult(snap)
		mutate(&v)
		if _, err := s.FreezeCollaborationDecision(u, r.ID, d.Revision, v); !errors.Is(err, ErrChatInvalid) {
			t.Fatal("invalid route accepted", err)
		}
	}
	if _, _, err := s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap)); !errors.Is(err, ErrChatConflict) {
		t.Fatal("cancelled task got an execution binding", err)
	}
}

func TestDecisionRestoreRetainsReservationWithoutDispatch(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "system", true: "full"}[full], func(t *testing.T) {
			s, u, in, cfg, snap := decisionFixture(t)
			r, d := acceptDecision(t, s, u, in, cfg, snap)
			if _, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000); err != nil {
				t.Fatal(err)
			}
			if err := s.RestoreCollaborations(full); err != nil {
				t.Fatal(err)
			}
			got, err := s.CollaborationDecision(u, r.ID)
			if err != nil || got.State != "interrupted" || got.Attempt.State != "unknown" || got.Attempt.ReservedMicroUSD != 1000 {
				t.Fatal(got, err)
			}
			if _, fresh, err := s.ClaimCollaborationDecision(u, r.ID, got.Revision); err != nil || fresh {
				t.Fatal("restore redispatched routing", fresh, err)
			}
			current, err := s.Collaboration(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if full {
				if current.State != CollabPaused || current.WaitReason != "instance_restored" {
					t.Fatal(current)
				}
			} else if current.State != CollabFailed || current.WaitReason != "artifacts_missing" {
				t.Fatal(current)
			}
		})
	}
}

func TestDecisionPauseFinishesRoutingButDoesNotStartExecution(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "freeze", true: "failure"}[fail], func(t *testing.T) {
			s, u, in, cfg, snap := decisionFixture(t)
			r, d := acceptDecision(t, s, u, in, cfg, snap)
			a, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "pause"}); err != nil {
				t.Fatal(err)
			}
			r, _ = s.Collaboration(u, r.ID)
			if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "supplement", Payload: json.RawMessage(`{"text":"do not rebuild routing"}`)}); !errors.Is(err, collaboration.ErrState) {
				t.Fatal("supplement bypassed routing boundary", err)
			}
			if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, CostMicroUSD: 42}, "jev-1.13.0"); err != nil {
				t.Fatal(err)
			}
			if fail {
				if err = s.FailCollaborationDecision(u, r.ID, d.Revision, "authorization_revoked"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap)); err != nil {
					t.Fatal(err)
				}
			}
			r, _ = s.Collaboration(u, r.ID)
			if r.State != CollabPaused || r.ResumeState != CollabAccepted {
				t.Fatal("routing resumed paused task", r)
			}
			if _, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision); !errors.Is(err, ErrChatConflict) {
				t.Fatal("paused task prepared", err)
			}
			if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "resume"}); err != nil {
				t.Fatal(err)
			}
			d, err = s.CollaborationDecision(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if d.State != "interrupted" {
					t.Fatal(d)
				}
				if _, err = s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap)); err != nil {
					t.Fatal(err)
				}
			} else if d.State != "frozen" {
				t.Fatal(d)
			}
			if _, fresh, err := s.ClaimCollaborationDecision(u, r.ID, d.Revision); err != nil || fresh {
				t.Fatal("pause/resume redispatched routing", fresh, err)
			}
			r, _ = s.Collaboration(u, r.ID)
			if _, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDecisionSnapshotTamperAndMissingGateFailClosed(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	snap.CandidateVersion = strings.Repeat("f", 64)
	if _, _, err := s.CreateCollaborationWithDecision(u, NewCollaborationID(), in, cfg, snap); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("invalid candidate digest accepted", err)
	}
	snap.CandidateVersion = ""
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	if _, err := s.db.Exec("UPDATE collaboration_decisions SET snapshot_json=json_set(snapshot_json,'$.roles.executor.candidates[0].binding.model','tampered') WHERE run_id=?", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CollaborationDecision(u, r.ID); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("changed candidates read as frozen", err)
	}
	if _, err := s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap)); !errors.Is(err, ErrChatInvalid) {
		t.Fatal("changed snapshot used", err)
	}
	if _, err := s.db.Exec("DELETE FROM collaboration_decisions WHERE run_id=?", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginCollaborationPreparation(u, r.ID, r.Revision); !errors.Is(err, collaboration.ErrState) {
		t.Fatal("missing routing row became legacy execution", err)
	}
}

func TestDecisionUnknownEvidenceNeverRegresses(t *testing.T) {
	for _, evidenceFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout_first", true: "evidence_first"}[evidenceFirst], func(t *testing.T) {
			s, u, in, cfg, snap := decisionFixture(t)
			r, d := acceptDecision(t, s, u, in, cfg, snap)
			a, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
			if err != nil {
				t.Fatal(err)
			}
			evidence := decision.Usage{Known: true, InputTokens: 1000, OutputTokens: 7, CostStatus: "unknown"}
			first, last := evidence, decision.Usage{CostStatus: "unknown"}
			firstModel, lastModel := "jev-1.14.0", ""
			if !evidenceFirst {
				first, last = last, first
				firstModel, lastModel = lastModel, firstModel
			}
			if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, first, firstModel); err != nil {
				t.Fatal(err)
			}
			if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, last, lastModel); err != nil {
				t.Fatal(err)
			}
			got, err := s.CollaborationDecision(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Attempt.State != "unknown" || got.Attempt.InputTokens != 1000 || got.Attempt.OutputTokens != 7 || got.Attempt.ActualModel != "jev-1.14.0" || got.Attempt.ReservedMicroUSD != 1000 {
				t.Fatal("late timeout erased provider evidence", got.Attempt)
			}
			if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, decision.Usage{CostStatus: "not_dispatched"}, ""); !errors.Is(err, ErrChatConflict) {
				t.Fatal("observed provider call became free", err)
			}
		})
	}
}

func TestDecisionFeeReconciliationUsesStoredEvidenceOnly(t *testing.T) {
	s, u, in, cfg, snap := decisionFixture(t)
	if _, err := s.Grant(u.Name, 10000, "reconcile-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	r, d := acceptDecision(t, s, u, in, cfg, snap)
	a, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
	if err != nil {
		t.Fatal(err)
	}
	usage := decision.Usage{Known: true, InputTokens: 1000, CostMicroUSD: 42, CostStatus: "known"}
	if _, err = s.db.Exec("CREATE TRIGGER fail_local_settlement BEFORE INSERT ON usage_events WHEN NEW.kind='decision' BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, usage, "jev-1.13.0"); err == nil {
		t.Fatal("expected settlement error")
	}
	if err = s.FailCollaborationDecisionSettlement(u, r.ID, a.ID, usage, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	got, err := s.CollaborationDecision(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "interrupted" || got.Attempt.ErrorCode != "decision_persistence_failed" || !got.Attempt.CanReconcile || got.Attempt.ReservedMicroUSD != 1000 {
		t.Fatal(got)
	}
	r, _ = s.Collaboration(u, r.ID)
	if r.State != CollabPaused || r.WaitReason != "decision_persistence_failed" {
		t.Fatal(r)
	}
	if err = s.ReconcileCollaborationDecisionAttempt(u, r.ID, r.Revision); err == nil {
		t.Fatal("still-failing ledger marked recovered")
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_local_settlement"); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileCollaborationDecisionAttempt(u, r.ID, r.Revision-1); !errors.Is(err, ErrChatConflict) {
		t.Fatal("stale recovery accepted", err)
	}
	if err = s.ReconcileCollaborationDecisionAttempt(u, r.ID, r.Revision); err != nil {
		t.Fatal(err)
	}
	got, err = s.CollaborationDecision(u, r.ID)
	if err != nil || got.Attempt.State != "settled" || got.Attempt.ErrorCode != "" || got.Attempt.CanReconcile || got.Attempt.CostMicroUSD != 42 {
		t.Fatal(got, err)
	}
	r, _ = s.Collaboration(u, r.ID)
	if r.State != CollabPaused || r.WaitReason != "decision_failed" {
		t.Fatal("reconciliation started business work", r)
	}
	if q, _ := s.GetQuota(u.Name); q.BalanceMicroUSD != 9958 {
		t.Fatal("bad recovery debit", q)
	}
	if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, usage, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	if len(s.ListUsage(UsageFilter{User: u.Name, Kind: UsageKindDecision})) != 1 {
		t.Fatal("reconciliation duplicated usage")
	}
}

func TestDecisionLateBillingPreservesPreparationAndCancellationClaims(t *testing.T) {
	for _, phase := range []string{"preparing", "cancelling", "cancelling_accounting_error"} {
		t.Run(phase, func(t *testing.T) {
			s, u, in, cfg, snap := decisionFixture(t)
			r, d := acceptDecision(t, s, u, in, cfg, snap)
			a, _, err := s.BeginCollaborationDecisionAttempt(u, r.ID, d.Revision, NewCollaborationID(), "jev", "jev-1.13.0", 1000)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, decision.Usage{CostStatus: "unknown"}, ""); err != nil {
				t.Fatal(err)
			}
			r, err = s.FreezeCollaborationDecision(u, r.ID, d.Revision, fallbackResult(snap))
			if err != nil {
				t.Fatal(err)
			}
			if phase == "preparing" {
				r, err = s.BeginCollaborationPreparation(u, r.ID, r.Revision)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if _, _, err = s.ApplyCollaborationAction(u, r.ID, NewCollaborationID(), CollaborationActionInput{Scope: r.Scope, ExpectedRevision: r.Revision, Action: "cancel"}); err != nil {
					t.Fatal(err)
				}
				r, err = s.Collaboration(u, r.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			beforeDecision, err := s.CollaborationDecision(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			known := decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, CostMicroUSD: 42}
			if phase == "cancelling_accounting_error" {
				if err = s.FailCollaborationDecisionSettlement(u, r.ID, a.ID, known, "jev-1.13.0"); err != nil {
					t.Fatal(err)
				}
			} else if err = s.SettleCollaborationDecisionAttempt(u, r.ID, a.ID, known, "jev-1.13.0"); err != nil {
				t.Fatal(err)
			}
			after, err := s.Collaboration(u, r.ID)
			if err != nil || after.Revision != r.Revision || after.State != r.State || after.WaitReason != r.WaitReason {
				t.Fatal("fee evidence invalidated lifecycle owner", r, after, err)
			}
			afterDecision, err := s.CollaborationDecision(u, r.ID)
			if err != nil || afterDecision.Revision != beforeDecision.Revision {
				t.Fatal("fee evidence invalidated routing claim", afterDecision, err)
			}
			if phase == "preparing" {
				if err = s.PinCollaborationImage(u, r.ID, r.Revision, "sha256:"+strings.Repeat("a", 64)); err != nil {
					t.Fatal("late fee prevented image pin", err)
				}
			} else {
				if err = s.ConfirmCollaborationStopped(u, r.ID, r.Revision, nil); err != nil {
					t.Fatal("late fee prevented quiescence acknowledgment", err)
				}
				if _, err = s.FinalizeCollaborationCancellation(u, r.ID, r.Revision); err != nil {
					t.Fatal("late fee left task cancelling", err)
				}
			}
		})
	}
}
