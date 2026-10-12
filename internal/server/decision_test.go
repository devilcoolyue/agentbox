package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
	"agentbox/internal/store"
)

type fixtureDecisionProvider func(context.Context, decision.Request) (decision.Evaluation, error)

func (fixtureDecisionProvider) ID() string { return "jev" }
func (fixtureDecisionProvider) Capabilities() decision.Capabilities {
	return decision.Capabilities{Choice: true}
}
func (f fixtureDecisionProvider) Evaluate(ctx context.Context, r decision.Request) (decision.Evaluation, error) {
	return f(ctx, r)
}

func decisionFixture(t *testing.T) (*Server, store.User, store.CollaborationCreateInput, http.Handler) {
	t.Helper()
	s, source, h := clientTestServer(t)
	t.Cleanup(func() { s.runtime().wg.Wait() })
	u, _ := s.store.GetUser(source.User)
	if err := s.cfg.SetCollaborationEnabled(true); err != nil {
		t.Fatal(err)
	}
	models := []config.ModelOption{{ID: "strong", Label: "Complex tasks"}, {ID: "economy", Label: "Economical small tasks"}}
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Models: &models}); err != nil {
		t.Fatal(err)
	}
	setDecisionPlugin(t, s, true, 1500)
	b := collaboration.Binding{AgentType: "claude", AccountID: "shared", Model: "strong"}
	in := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: source.ID, Text: "Fix this bounded task", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: collaboration.Roles{Coordinator: b, Executor: b}, AutoExecute: true, MaxSteps: 20, BudgetMicroUSD: 10000, Decision: &decision.Intent{Version: 1, Provider: "jev", Roles: map[collaboration.Role]decision.RoleIntent{collaboration.Coordinator: {Mode: "fixed"}, collaboration.Executor: {Mode: "auto"}}}}
	return s, u, in, h
}

func setDecisionPlugin(t *testing.T, s *Server, enabled bool, timeout int) {
	t.Helper()
	revision := s.cfg.GetDecision().Revision
	err := s.cfg.ApplySettings(config.SettingsPatch{Decision: &config.DecisionPatch{ExpectedRevision: &revision, Plugins: &config.DecisionPluginsPatch{Jev: &config.DecisionJevPatch{Enabled: &enabled, TimeoutMS: &timeout, Access: &config.AccountAccess{Mode: "all"}, APIKey: &config.DecisionSecretPatch{Action: "set", Value: "synthetic-key-never-network"}}}}})
	if err != nil {
		t.Fatal(err)
	}
}

func acceptDecisionFixture(t *testing.T, s *Server, u store.User, in store.CollaborationCreateInput) store.CollaborationRun {
	t.Helper()
	snapshot, err := s.collaborationDecisionSnapshot(u, in)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.CollaborationConfig{Roles: in.Roles, AutoExecute: in.AutoExecute, MaxSteps: in.MaxSteps, BudgetMicroUSD: in.BudgetMicroUSD}
	run, fresh, err := s.store.CreateCollaborationWithDecision(u, store.NewCollaborationID(), in, cfg, snapshot)
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	return run
}

func knownDecision(req decision.Request) decision.Evaluation {
	out := decision.Evaluation{Provider: "jev", Model: config.DecisionJevModel, Status: "suggest", Dispatched: true, Usage: decision.Usage{Known: true, CostStatus: "known", InputTokens: 1000, CostMicroUSD: 42}, Suggestions: map[collaboration.Role]decision.Suggestion{}}
	for role, candidates := range req.Roles {
		for _, c := range candidates {
			if c.Binding.Model == "economy" {
				out.Suggestions[role] = decision.Suggestion{Status: "suggest", CandidateRef: c.Ref}
				break
			}
		}
	}
	return out
}

func TestDecisionFallbackKeepsConfiguredCapabilityDescription(t *testing.T) {
	s, u, in, _ := decisionFixture(t)
	snapshot, err := s.collaborationDecisionSnapshot(u, in)
	if err != nil {
		t.Fatal(err)
	}
	role := snapshot.Roles[collaboration.Executor]
	if role.Fallback != in.Roles.Executor || len(role.Candidates) != 2 {
		t.Fatal("candidate set or fallback changed", role)
	}
	for _, candidate := range role.Candidates {
		if candidate.Binding.Model == "strong" && !strings.Contains(candidate.Description, "Complex tasks") {
			t.Fatal("fallback lost configured capability evidence", candidate.Description)
		}
		if candidate.Binding.Model == "economy" && !strings.Contains(candidate.Description, "Economical small tasks") {
			t.Fatal("alternative lost configured capability evidence", candidate.Description)
		}
	}
}

func TestDecisionFixedAndOptionalPathsDoNotCallProvider(t *testing.T) {
	for _, scenario := range []string{"fixed", "disabled", "builtin", "denied", "unique", "budget", "config_changed"} {
		t.Run(scenario, func(t *testing.T) {
			s, u, in, _ := decisionFixture(t)
			var calls atomic.Int32
			s.decisionProvider = func(config.DecisionJev) decision.Provider {
				return fixtureDecisionProvider(func(context.Context, decision.Request) (decision.Evaluation, error) {
					calls.Add(1)
					return decision.Evaluation{}, errors.New("unexpected invocation")
				})
			}
			switch scenario {
			case "fixed":
				in.Decision.Roles[collaboration.Executor] = decision.RoleIntent{Mode: "fixed"}
			case "disabled":
				setDecisionPlugin(t, s, false, 1500)
			case "builtin":
				in.Decision.Provider = "builtin"
			case "denied":
				rev := s.cfg.GetDecision().Revision
				if err := s.cfg.ApplySettings(config.SettingsPatch{Decision: &config.DecisionPatch{ExpectedRevision: &rev, Plugins: &config.DecisionPluginsPatch{Jev: &config.DecisionJevPatch{Access: &config.AccountAccess{Mode: "admin"}}}}}); err != nil {
					t.Fatal(err)
				}
			case "unique":
				models := []config.ModelOption{{ID: "strong"}}
				if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Models: &models}); err != nil {
					t.Fatal(err)
				}
			case "budget":
				in.BudgetMicroUSD = 1
			}
			run := acceptDecisionFixture(t, s, u, in)
			if scenario == "config_changed" {
				setDecisionPlugin(t, s, true, 2000)
			}
			ready, err := s.resolveCollaborationDecision(context.Background(), u, run)
			if err != nil || !ready || calls.Load() != 0 {
				t.Fatal(ready, err, calls.Load())
			}
			d, err := s.store.CollaborationDecision(u, run.ID)
			if err != nil || d.State != "frozen" || d.Result.Roles != in.Roles || d.Attempt != nil {
				t.Fatal(d, err)
			}
		})
	}
}

func TestDecisionOnlyUnresolvedRolesAndOneClaim(t *testing.T) {
	for _, scenario := range []string{"fixed_coordinator", "fixed_executor", "builtin_coordinator", "both_auto"} {
		t.Run(scenario, func(t *testing.T) {
			s, u, in, h := decisionFixture(t)
			if scenario == "fixed_executor" {
				in.Decision.Roles[collaboration.Coordinator] = decision.RoleIntent{Mode: "auto"}
				in.Decision.Roles[collaboration.Executor] = decision.RoleIntent{Mode: "fixed"}
			}
			if scenario == "both_auto" || scenario == "builtin_coordinator" {
				in.Decision.Roles[collaboration.Coordinator] = decision.RoleIntent{Mode: "auto"}
			}
			if scenario == "builtin_coordinator" {
				if err := s.cfg.AddAccount(config.Account{ID: "single", Type: "codex", Models: []config.ModelOption{{ID: "strong"}}}); err != nil {
					t.Fatal(err)
				}
				in.Roles.Coordinator = collaboration.Binding{AgentType: "codex", AccountID: "single", Model: "strong"}
			}
			var calls atomic.Int32
			s.decisionProvider = func(config.DecisionJev) decision.Provider {
				return fixtureDecisionProvider(func(_ context.Context, req decision.Request) (decision.Evaluation, error) {
					calls.Add(1)
					want := 1
					if scenario == "both_auto" {
						want = 2
					}
					if len(req.Roles) != want {
						t.Errorf("sent resolved role: %+v", req.Roles)
					}
					if scenario == "builtin_coordinator" || scenario == "fixed_coordinator" {
						if _, sent := req.Roles[collaboration.Coordinator]; sent {
							t.Error("sent coordinator")
						}
					}
					return knownDecision(req), nil
				})
			}
			run := acceptDecisionFixture(t, s, u, in)
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := s.resolveCollaborationDecision(context.Background(), u, run)
					if err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("calls=%d", calls.Load())
			}
			d, err := s.store.CollaborationDecision(u, run.ID)
			if err != nil || d.State != "frozen" {
				t.Fatal(d, err)
			}
			for role, policy := range in.Decision.Roles {
				chosen, _ := d.Result.Roles.Binding(role)
				original, _ := in.Roles.Binding(role)
				if policy.Mode == "fixed" || scenario == "builtin_coordinator" && role == collaboration.Coordinator {
					if chosen != original {
						t.Fatal("fixed/builtin role changed", role, chosen)
					}
				} else if chosen.Model != "economy" {
					t.Fatal(chosen)
				}
			}
			raw, _ := json.Marshal(in)
			w := clientRequest(h, u.Name, "PUT", "/api/collaboration/runs/"+run.RequestID, string(raw))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"replayed":true`) {
				t.Fatal(w.Code, w.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatal("replay invoked plugin")
			}
			original, _ := s.store.Collaboration(u, run.ID)
			if original.Request.Roles != in.Roles {
				t.Fatal("accepted intent rewritten")
			}
			cost, err := s.store.CollaborationCost(u, run.ID)
			if err != nil || cost.CostMicroUSD != 42 || cost.DecisionReservedMicroUSD != 0 {
				t.Fatal(cost, err)
			}
		})
	}
}

func TestDecisionTimeoutDisableAndLateFeesNeverReselect(t *testing.T) {
	for _, scenario := range []string{"timeout", "disable", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			s, u, in, _ := decisionFixture(t)
			setDecisionPlugin(t, s, true, 100)
			started, finish := make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(finish) })
			s.decisionProvider = func(config.DecisionJev) decision.Provider {
				return fixtureDecisionProvider(func(_ context.Context, req decision.Request) (decision.Evaluation, error) {
					close(started)
					<-finish
					return knownDecision(req), nil
				})
			}
			run := acceptDecisionFixture(t, s, u, in)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := s.resolveCollaborationDecision(ctx, u, run); done <- err }()
			<-started
			if scenario == "disable" {
				setDecisionPlugin(t, s, false, 100)
			}
			if scenario == "cancel" {
				current, _ := s.store.Collaboration(u, run.ID)
				_, _, err := s.store.ApplyCollaborationAction(u, run.ID, store.NewCollaborationID(), store.CollaborationActionInput{Scope: in.Scope, ExpectedRevision: current.Revision, Action: "cancel"})
				if err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			if err := <-done; scenario != "cancel" && err != nil {
				t.Fatal(err)
			}
			d, err := s.store.CollaborationDecision(u, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cancel" {
				if d.Result != nil {
					t.Fatal("cancelled intent frozen")
				}
			} else if d.State != "frozen" || d.Result.Roles != in.Roles {
				t.Fatal("fallback not frozen", d)
			}
			before := d.Result
			cost, _ := s.store.CollaborationCost(u, run.ID)
			if cost.DecisionReservedMicroUSD != 2753 || cost.CostMicroUSD != 0 {
				t.Fatal("ambiguous timeout freed reservation", cost)
			}
			release.Do(func() { close(finish) })
			s.runtime().wg.Wait()
			after, _ := s.store.CollaborationDecision(u, run.ID)
			first, _ := json.Marshal(before)
			last, _ := json.Marshal(after.Result)
			if string(first) != string(last) {
				t.Fatal("late fee evidence rerouted task")
			}
			cost, _ = s.store.CollaborationCost(u, run.ID)
			if cost.DecisionReservedMicroUSD != 0 || cost.CostMicroUSD != 42 {
				t.Fatal(cost)
			}
			usage := s.store.ListUsage(store.UsageFilter{User: u.Name, Kind: store.UsageKindDecision})
			if len(usage) != 1 {
				t.Fatal("fee not exactly once", len(usage))
			}
		})
	}
}

func TestDecisionCandidatesKeepConstraintsAndDoNotExpand(t *testing.T) {
	s, u, in, _ := decisionFixture(t)
	supported := &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"high"}}
	models := []config.ModelOption{{ID: "strong", Reasoning: supported}, {ID: "economy", Reasoning: &config.ReasoningCapability{Support: "unsupported"}}}
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Models: &models}); err != nil {
		t.Fatal(err)
	}
	in.Roles.Executor.Effort = "high"
	in.Roles.Executor.EffortControl = "effort"
	run := acceptDecisionFixture(t, s, u, in)
	models = append(models, config.ModelOption{ID: "added-later", Reasoning: supported})
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Models: &models}); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.resolveCollaborationDecision(context.Background(), u, run); err != nil || !ready {
		t.Fatal(ready, err)
	}
	d, _ := s.store.CollaborationDecision(u, run.ID)
	if len(d.Snapshot.Roles[collaboration.Executor].Candidates) != 1 || d.Result.Roles != in.Roles {
		t.Fatal("constraints expanded", d)
	}
}

func TestDecisionDefaultsCapabilityAndNewProtocolValidation(t *testing.T) {
	_, u, in, h := decisionFixture(t)
	path := "/api/sessions/" + in.SourceSessionID + "/collaboration/decision-default"
	for _, provider := range []string{"jev", "builtin", "inherit"} {
		raw, _ := json.Marshal(map[string]any{"scope": in.Scope, "provider": provider})
		w := clientRequest(h, u.Name, "PUT", path, string(raw))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"provider":"`+provider+`"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := clientRequest(h, "bob", "GET", path, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	w := clientRequest(h, u.Name, "GET", "/api/collaboration/options", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"automatic_intent_version":1`) || strings.Contains(w.Body.String(), "synthetic-key") {
		t.Fatal(w.Code, w.Body.String())
	}
	in.Decision.Version = 2
	raw, _ := json.Marshal(in)
	w = clientRequest(h, u.Name, "PUT", "/api/collaboration/runs/"+store.NewCollaborationID(), string(raw))
	if w.Code != 400 {
		t.Fatal("unknown protocol accepted", w.Code, w.Body.String())
	}
}

func TestDecisionRevokedDuringAdviceDoesNotRun(t *testing.T) {
	s, u, in, _ := decisionFixture(t)
	s.decisionProvider = func(config.DecisionJev) decision.Provider {
		return fixtureDecisionProvider(func(_ context.Context, req decision.Request) (decision.Evaluation, error) {
			_, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Access: &config.AccountAccess{Mode: "admin"}})
			if err != nil {
				t.Error(err)
			}
			return knownDecision(req), nil
		})
	}
	run := acceptDecisionFixture(t, s, u, in)
	if ready, err := s.resolveCollaborationDecision(context.Background(), u, run); !errors.Is(err, errAccountAccess) || ready {
		t.Fatal(ready, err)
	}
	run, _ = s.store.Collaboration(u, run.ID)
	if run.State != store.CollabPaused || run.WaitReason != "authorization_revoked" {
		t.Fatal(run)
	}
}

func TestDecisionRestartNeedsExplicitResumeAndNeverReclassifies(t *testing.T) {
	s, u, in, _ := decisionFixture(t)
	run := acceptDecisionFixture(t, s, u, in)
	if err := s.store.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	run, _ = s.store.Collaboration(u, run.ID)
	if run.State != store.CollabPaused {
		t.Fatal(run)
	}
	_, fresh, err := s.store.ApplyCollaborationAction(u, run.ID, store.NewCollaborationID(), store.CollaborationActionInput{Scope: in.Scope, ExpectedRevision: run.Revision, Action: "resume"})
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	run, _ = s.store.Collaboration(u, run.ID)
	if ready, err := s.resolveCollaborationDecision(context.Background(), u, run); err != nil || !ready {
		t.Fatal(ready, err)
	}
	d, _ := s.store.CollaborationDecision(u, run.ID)
	if d.Attempt != nil || d.Result.Roles != in.Roles || d.Result.Decisions[collaboration.Executor].Reason != "recovered_fallback" {
		t.Fatal(d)
	}
}

func TestDecisionProviderPanicKeepsReservationAndFallback(t *testing.T) {
	s, u, in, _ := decisionFixture(t)
	s.decisionProvider = func(config.DecisionJev) decision.Provider {
		return fixtureDecisionProvider(func(context.Context, decision.Request) (decision.Evaluation, error) {
			panic("synthetic adapter failure")
		})
	}
	run := acceptDecisionFixture(t, s, u, in)
	if ready, err := s.resolveCollaborationDecision(context.Background(), u, run); !ready || err != nil {
		t.Fatal(ready, err)
	}
	s.runtime().wg.Wait()
	d, _ := s.store.CollaborationDecision(u, run.ID)
	if d.Result.Roles != in.Roles || d.Attempt.State != "unknown" || d.Attempt.ReservedMicroUSD != 2753 {
		t.Fatal(d)
	}
}

func TestDecisionAccountingFailureReconcilesWithoutModelReplay(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "before_freeze"
		if late {
			name = "late_evidence"
		}
		t.Run(name, func(t *testing.T) {
			s, u, in, h := decisionFixture(t)
			setDecisionPlugin(t, s, true, 100)
			// Only this newly allocated synthetic test database can install the
			// failure trigger. No helper accepts an existing instance/database.
			db, err := sql.Open("sqlite", filepath.Join(s.cfg.DataDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE decision_synthetic_only(id INTEGER PRIMARY KEY CHECK(id=1)); INSERT INTO decision_synthetic_only VALUES(1); CREATE TRIGGER decision_usage_failure BEFORE INSERT ON usage_events WHEN NEW.kind='decision' AND EXISTS(SELECT 1 FROM decision_synthetic_only WHERE id=1) BEGIN SELECT RAISE(ABORT,'synthetic decision ledger failure'); END`); err != nil {
				t.Fatal(err)
			}
			finish := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(finish) })
			var calls atomic.Int32
			s.decisionProvider = func(config.DecisionJev) decision.Provider {
				return fixtureDecisionProvider(func(_ context.Context, req decision.Request) (decision.Evaluation, error) {
					calls.Add(1)
					if late {
						<-finish
					}
					return knownDecision(req), nil
				})
			}
			run := acceptDecisionFixture(t, s, u, in)
			ready, resolveErr := s.resolveCollaborationDecision(context.Background(), u, run)
			if late && (!ready || resolveErr != nil) {
				t.Fatal(ready, resolveErr)
			}
			if !late && (ready || !errors.Is(resolveErr, errDecisionAccounting)) {
				t.Fatal(ready, resolveErr)
			}
			release.Do(func() { close(finish) })
			s.runtime().wg.Wait()
			d, err := s.store.CollaborationDecision(u, run.ID)
			if err != nil || d.Attempt.ErrorCode != "decision_persistence_failed" || d.Attempt.ReservedMicroUSD != 2753 {
				t.Fatal(d, err)
			}
			current, _ := s.store.Collaboration(u, run.ID)
			if current.State != store.CollabPaused || current.WaitReason != "decision_persistence_failed" {
				t.Fatal(current)
			}
			if _, _, err := s.store.ApplyCollaborationAction(u, run.ID, store.NewCollaborationID(), store.CollaborationActionInput{Scope: in.Scope, ExpectedRevision: current.Revision, Action: "resume"}); err == nil {
				t.Fatal("resumed with accounting failure")
			}
			if _, err := db.Exec("DROP TRIGGER decision_usage_failure"); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"scope": in.Scope, "expected_revision": current.Revision})
			w := clientRequest(h, u.Name, "POST", "/api/collaboration/runs/"+run.ID+"/decision/reconcile", string(raw))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			current, _ = s.store.Collaboration(u, run.ID)
			if current.State != store.CollabPaused {
				t.Fatal("fee reconcile auto started business task")
			}
			if _, _, err := s.store.ApplyCollaborationAction(u, run.ID, store.NewCollaborationID(), store.CollaborationActionInput{Scope: in.Scope, ExpectedRevision: current.Revision, Action: "resume"}); err != nil {
				t.Fatal(err)
			}
			current, _ = s.store.Collaboration(u, run.ID)
			if ready, err := s.resolveCollaborationDecision(context.Background(), u, current); !ready || err != nil {
				t.Fatal(ready, err)
			}
			d, _ = s.store.CollaborationDecision(u, run.ID)
			cost, err := s.store.CollaborationCost(u, run.ID)
			if err != nil || calls.Load() != 1 || d.Result.Roles != in.Roles || cost.CostMicroUSD != 42 || cost.DecisionReservedMicroUSD != 0 {
				t.Fatal(calls.Load(), d, cost, err)
			}
		})
	}
}
