package server

import (
	"context"
	"errors"
	"log"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/decision"
	"agentbox/internal/store"
)

var errDecisionAccounting = errors.New("decision settlement requires inspection")

// resolveCollaborationDecision grants no execution rights. Both bindings are
// frozen before the existing preparation/step claims may admit any CLI work.
func (s *Server) resolveCollaborationDecision(ctx context.Context, u store.User, run store.CollaborationRun) (ready bool, resultErr error) {
	if run.Request == nil || run.Request.Decision == nil {
		return true, nil
	}
	record, err := s.store.CollaborationDecision(u, run.ID)
	if err != nil {
		return false, err
	}
	if record.State == "frozen" {
		return true, nil
	}
	recovered := record.State == "interrupted"
	if !recovered {
		var fresh bool
		record, fresh, err = s.store.ClaimCollaborationDecision(u, run.ID, record.Revision)
		if err != nil || !fresh {
			return record.State == "frozen", err
		}
	}
	defer func() {
		if resultErr == nil || ctx.Err() != nil {
			return
		}
		reason := "decision_failed"
		if errors.Is(resultErr, errAccountAccess) {
			reason = "authorization_revoked"
		}
		if errors.Is(resultErr, errDecisionAccounting) {
			reason = "decision_persistence_failed"
		}
		// Cancellation, a newer revision, or another completed freeze defeats
		// this CAS. A core error must never be treated as plugin unavailability.
		e := s.store.FailCollaborationDecision(u, run.ID, record.Revision, reason)
		if !errors.Is(e, store.ErrChatConflict) {
			resultErr = errors.Join(resultErr, e)
		}
	}()
	if record.Snapshot == nil {
		return false, decision.ErrInvalid
	}
	snapshot := *record.Snapshot
	result := decision.Result{Decisions: make(map[collaboration.Role]decision.Decision)}
	unresolved := make(map[collaboration.Role][]decision.Candidate)
	current, ok := s.store.GetUser(u.Name)
	if !ok || !current.CreatedAt.Equal(u.CreatedAt) {
		return false, errAccountAccess
	}
	cfg := s.cfg.CloneForDecision()
	for _, role := range []collaboration.Role{collaboration.Coordinator, collaboration.Executor} {
		r := snapshot.Roles[role]
		if run.Request.Workflow.IsDirect() && role == collaboration.Coordinator {
			if r.Mode != "fixed" {
				return false, decision.ErrInvalid
			}
			result.Decisions[role] = decision.Decision{Binding: r.Binding, Source: "manual", Reason: "inactive_role"}
			continue
		}
		if recovered && r.Mode == "auto" {
			result.Decisions[role] = decision.Decision{Binding: r.Fallback, Source: "fallback", Reason: "recovered_fallback"}
			continue
		}
		// Revocation only narrows the accepted candidate pool. New accounts or
		// models added after acceptance cannot enter this decision.
		if r.Mode == "auto" {
			r.Candidates = nil
			for _, c := range snapshot.Roles[role].Candidates {
				if validateDecisionBinding(cfg, current, c.Binding) == nil {
					r.Candidates = append(r.Candidates, c)
				}
			}
		}
		b, reason, resolved := (decision.Builtin{}).Resolve(r)
		if resolved {
			source := "builtin"
			if r.Mode == "fixed" {
				source = "manual"
			}
			result.Decisions[role] = decision.Decision{Binding: b, Source: source, Reason: reason}
		} else {
			unresolved[role] = r.Candidates
		}
	}
	if len(unresolved) > 0 {
		evaluation, fallbackReason, e := s.evaluateCollaborationDecision(ctx, current, run, record, unresolved)
		if e != nil {
			return false, e
		}
		result.ProviderModel = evaluation.Model
		for role, candidates := range unresolved {
			resolved := decision.Decision{Binding: snapshot.Roles[role].Fallback, Source: "fallback", Reason: fallbackReason}
			suggestion := evaluation.Suggestions[role]
			if suggestion.Status == "suggest" {
				for _, candidate := range candidates {
					if candidate.Ref == suggestion.CandidateRef {
						resolved = decision.Decision{Binding: candidate.Binding, Source: "jev", Reason: "provider_suggestion"}
						break
					}
				}
			}
			result.Decisions[role] = resolved
		}
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	current, ok = s.store.GetUser(u.Name)
	if !ok || !current.CreatedAt.Equal(u.CreatedAt) {
		return false, errAccountAccess
	}
	cfg = s.cfg.CloneForDecision()
	for _, role := range []collaboration.Role{collaboration.Coordinator, collaboration.Executor} {
		d := result.Decisions[role]
		if run.Request.Workflow.IsDirect() && role == collaboration.Coordinator {
			result.Roles.Coordinator = d.Binding
			continue
		}
		if validateDecisionBinding(cfg, current, d.Binding) != nil && snapshot.Roles[role].Mode == "auto" {
			d = decision.Decision{Binding: snapshot.Roles[role].Fallback, Source: "fallback", Reason: "authorization_fallback"}
		}
		if err = validateDecisionBinding(cfg, current, d.Binding); err != nil {
			return false, errAccountAccess
		}
		result.Decisions[role] = d
		if role == collaboration.Coordinator {
			result.Roles.Coordinator = d.Binding
		} else {
			result.Roles.Executor = d.Binding
		}
	}
	_, err = s.store.FreezeCollaborationDecision(u, run.ID, record.Revision, result)
	return err == nil, err
}

func (s *Server) evaluateCollaborationDecision(ctx context.Context, u store.User, run store.CollaborationRun, record store.CollaborationDecisionRecord, roles map[collaboration.Role][]decision.Candidate) (decision.Evaluation, string, error) {
	empty := decision.Evaluation{Usage: decision.Usage{CostStatus: "not_dispatched"}}
	if record.Snapshot.Provider != "jev" {
		return empty, "builtin_fallback", nil
	}
	cfg, changed := s.cfg.DecisionState()
	if cfg.Revision != record.Snapshot.ConfigRevision {
		return empty, "configuration_changed", nil
	}
	if !cfg.Plugins.Jev.Enabled || cfg.Plugins.Jev.APIKey == "" || !cfg.Plugins.Jev.CanUse(u.Name, u.Role == store.RoleAdmin) {
		return empty, "provider_unavailable", nil
	}
	provider := decision.NewJev(cfg.Plugins.Jev)
	request := decision.Request{ID: store.NewCollaborationID(), Purpose: "collaboration_model_selection", TemplateVersion: record.Snapshot.TemplateVersion, State: decisionTaskState(*run.Request), Roles: roles}
	if e := provider.ValidateRequest(request); e != nil {
		return empty, "request_limit", nil
	}
	bound, e := provider.ReservedMicroUSD()
	if e != nil {
		return empty, "budget_unavailable", nil
	}
	release, e := s.decisionService.Acquire(cfg.Revision, cfg.Plugins.Jev.MaxConcurrency)
	if e != nil {
		return empty, "provider_busy", nil
	}
	// Ownership of the acquired slot moves to the provider worker only after
	// all durable/local gates succeed; no slot is held by the fallback CLI.
	owned := true
	defer func() {
		if owned {
			release(false)
		}
	}()
	attempt, fresh, e := s.store.BeginCollaborationDecisionAttempt(u, run.ID, record.Revision, request.ID, provider.ID(), cfg.Plugins.Jev.Model, bound)
	if errors.Is(e, decision.ErrBudgetUnknown) {
		return empty, "budget_unavailable", nil
	}
	if e != nil {
		return empty, "", e
	}
	if !fresh {
		return empty, "", store.ErrChatConflict
	}
	current, currentChanged := s.cfg.DecisionState()
	if current.Revision != cfg.Revision || currentChanged != changed || ctx.Err() != nil {
		e = s.store.SettleCollaborationDecisionAttempt(u, run.ID, attempt.ID, empty.Usage, "")
		return empty, "configuration_changed", errors.Join(e, ctx.Err())
	}
	var evaluator decision.Provider = provider
	if s.decisionProvider != nil {
		evaluator = s.decisionProvider(cfg.Plugins.Jev)
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Plugins.Jev.TimeoutMS)*time.Millisecond)
	defer cancel()
	type response struct {
		evaluation     decision.Evaluation
		err, settleErr error
	}
	done := make(chan response, 1)
	if !s.spawn(func() {
		evaluation, callErr := invokeDecisionProvider(callCtx, changed, evaluator, request)
		defer release(evaluation.Dispatched && callErr != nil && !errors.Is(callErr, context.Canceled))
		settleErr := s.settleDecisionEvidence(u, run.ID, attempt.ID, evaluation)
		if settleErr != nil {
			log.Printf("decision accounting requires inspection run=%s attempt=%s", run.ID, attempt.ID)
		}
		done <- response{evaluation, callErr, settleErr}
	}) {
		e = s.store.SettleCollaborationDecisionAttempt(u, run.ID, attempt.ID, empty.Usage, "")
		return empty, "", errors.Join(context.Canceled, e)
	}
	owned = false
	select {
	case response := <-done:
		if response.settleErr != nil {
			return empty, "", errors.Join(errDecisionAccounting, response.settleErr)
		}
		// Changes/cancellation racing a successful return retain fee evidence
		// but cannot change bindings using the old provider generation.
		select {
		case <-changed:
			return empty, "configuration_changed", nil
		default:
		}
		if ctx.Err() != nil {
			return empty, "", ctx.Err()
		}
		if response.err != nil {
			return empty, "provider_error", nil
		}
		return response.evaluation, "provider_abstained", nil
	case <-changed:
		cancel()
		e = s.store.SettleCollaborationDecisionAttempt(u, run.ID, attempt.ID, decision.Usage{CostStatus: "unknown"}, "")
		if e != nil {
			return empty, "", errors.Join(errDecisionAccounting, e)
		}
		return empty, "configuration_changed", nil
	case <-callCtx.Done():
		e = s.store.SettleCollaborationDecisionAttempt(u, run.ID, attempt.ID, decision.Usage{CostStatus: "unknown"}, "")
		if ctx.Err() != nil {
			return empty, "", errors.Join(ctx.Err(), e)
		}
		if e != nil {
			return empty, "", errors.Join(errDecisionAccounting, e)
		}
		return empty, "provider_timeout", nil
	}
}

// Compiled adapters are replaceable; a panic is an ambiguous paid attempt,
// never permission to crash the service, lose the reservation or dispatch again.
func invokeDecisionProvider(ctx context.Context, changed <-chan struct{}, provider decision.Provider, request decision.Request) (evaluation decision.Evaluation, err error) {
	evaluation.Usage.CostStatus = "not_dispatched"
	select {
	case <-changed:
		return evaluation, decision.ErrGeneration
	case <-ctx.Done():
		return evaluation, ctx.Err()
	default:
	}
	evaluation.Usage.CostStatus = "unknown"
	evaluation.Dispatched = true
	defer func() {
		if recover() != nil {
			err = decision.ErrUnavailable
		}
	}()
	return provider.Evaluate(ctx, request)
}

func (s *Server) settleDecisionEvidence(u store.User, run, attempt string, evaluation decision.Evaluation) error {
	var err error
	// Only local, idempotent accounting may retry. The HTTP classification is
	// never retried, including when its successful response cannot be settled.
	for n := 0; n < 3; n++ {
		err = s.store.SettleCollaborationDecisionAttempt(u, run, attempt, evaluation.Usage, evaluation.Model)
		if err == nil {
			return nil
		}
		if errors.Is(err, store.ErrChatInvalid) || errors.Is(err, store.ErrChatConflict) {
			break
		}
		if n < 2 {
			time.Sleep(time.Duration(n+1) * 20 * time.Millisecond)
		}
	}
	markErr := s.store.FailCollaborationDecisionSettlement(u, run, attempt, evaluation.Usage, evaluation.Model)
	// A late accounting failure must prevent new business steps. An already
	// running CLI is interrupted through its original runtime and may become
	// uncertain; a fee failure is never recast as ordinary plugin fallback.
	if d, readErr := s.store.CollaborationDecision(u, run); readErr == nil && d.Result != nil {
		s.cancelCollaborationJob(run)
	}
	return errors.Join(errDecisionAccounting, err, markErr)
}
