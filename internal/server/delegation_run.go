package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/decision"
	"agentbox/internal/delegation"
	"agentbox/internal/protocol"
	"agentbox/internal/store"
)

// execute runs the chosen worker against the owner's workspace and records
// the outcome. Workers on the main agent's own account and CLI run beside it
// in the same container; any other account or CLI gets a hidden worker
// workspace whose /workspace is the owner's directory.
func (g *delegationGrant) execute(ctx context.Context, call *delegationCall, d store.Delegation, route delegation.Route, routeJSON []byte, worker delegation.Worker, prev store.Delegation, task delegation.Task, started time.Time) delegationResult {
	s := g.srv
	fail := func(d store.Delegation, code, msg string) delegationResult {
		if d.State == store.DelegationRouting || d.State == store.DelegationRunning {
			if done, err := s.store.FinishDelegation(d.ID, store.DelegationFailed, "", nil, code); err == nil {
				d = done
			}
		}
		g.publish(d, true)
		return delegationResult{DelegationID: d.ID, Status: "failed", Worker: worker.Label(), Route: routeSummary(route), Error: msg,
			CostUSD: float64(s.store.DelegationCost(d.ID)) / 1e6, DurationS: int64(time.Since(started).Seconds())}
	}
	owner, ok := s.store.Get(g.session)
	if !ok {
		return fail(d, "session_not_found", "the workspace no longer exists")
	}
	same := worker.AccountID == owner.AccountID && worker.Agent == owner.Agent
	exec := owner
	if !same {
		ws, ok := s.store.WorkerSession(owner.ID, worker.AccountID)
		if !ok {
			var err error
			ws, err = s.workspaces().CreateWorker(ctx, owner, store.Session{ID: store.NewID(), User: owner.User, Name: "delegation worker",
				Agent: worker.Agent, AccountID: worker.AccountID, DefaultModel: worker.Model, Status: store.StatusStopped})
			if err != nil {
				log.Printf("delegation %s worker workspace: %v", d.ID, err)
				return fail(d, "worker_workspace_failed", "could not prepare the worker workspace")
			}
		}
		exec = ws
	}
	binding := store.Delegation{Agent: worker.Agent, AccountID: worker.AccountID, Model: worker.Model}
	if !same {
		binding.WorkerSessionID = exec.ID
	}
	d, err := s.store.RouteDelegation(d.ID, routeJSON, binding, false)
	if err != nil {
		return delegationResult{DelegationID: call.id, Status: "failed", Error: "could not record the routing decision"}
	}
	g.publish(d, true)

	remaining := time.Until(g.deadline) - 2*time.Minute
	timeout := min(delegationWorkerTimeout, remaining)
	if timeout < time.Minute {
		return fail(d, "turn_time_exhausted", "not enough time left in this turn for a worker")
	}
	before := s.delegationSnapshot(ctx, owner)
	t := &delegationTurn{g: g, call: call, d: d, owner: owner, exec: exec, same: same, worker: worker, turnID: store.NewID(),
		pidFile: "/tmp/.agentbox-delegate-" + d.ID + ".pid"}
	call.mu.Lock()
	call.interrupt = t.interrupt
	call.mu.Unlock()

	text := delegation.WorkerInput(task, prev.ID != "")
	in := chat.Input{SessionID: exec.ID, Text: text, Model: worker.Model, Timeout: timeout, Fresh: true, Resume: prev.ProviderSession, RequireUsage: true,
		Meter: func(e *store.UsageEvent) { e.SessionID, e.ThreadID, e.DelegationID = owner.ID, g.thread, d.ID }}
	if worker.Agent == config.AgentClaude {
		in.Extras = agent.ChatExtras{PidFile: t.pidFile, AppendSystemPrompt: delegation.WorkerPrompt(task)}
	} else {
		in.Text = delegation.WorkerPrompt(task) + "\n\n" + text
	}
	service := chat.Service{Runtime: delegationRuntime{t}, Usage: s.usageService()}
	service.Run(ctx, in, t)

	after := s.delegationSnapshot(context.WithoutCancel(ctx), owner)
	changes := delegation.Diff(before, after)
	changesJSON, _ := json.Marshal(changes)
	state, code := store.DelegationCompleted, ""
	switch {
	case t.Succeeded():
	case t.Interrupted() || errors.Is(ctx.Err(), context.Canceled):
		state, code = store.DelegationCancelled, "cancelled"
	default:
		state, code = store.DelegationFailed, t.code
		if code == "" {
			code = "worker_failed"
		}
	}
	report := t.report.Text()
	if done, err := s.store.FinishDelegation(d.ID, state, report, changesJSON, code); err == nil {
		d = done
	} else {
		log.Printf("delegation %s finish: %v", d.ID, err)
	}
	g.publish(d, true)
	res := delegationResult{DelegationID: d.ID, Status: state, Worker: fmt.Sprintf("%s (%s, %s)", worker.Label(), worker.Agent, worker.AccountLabel),
		Route: routeSummary(route), Report: report, Changes: &changes, CostUSD: float64(s.store.DelegationCost(d.ID)) / 1e6, DurationS: int64(time.Since(started).Seconds())}
	if state != store.DelegationCompleted {
		res.Error = "worker did not finish: " + code
	}
	if report == "" && state == store.DelegationCompleted {
		res.Report = "(the worker returned no report)"
	}
	return res
}

func (s *Server) delegationSnapshot(ctx context.Context, owner store.Session) delegation.Manifest {
	root, err := s.openDataDir(s.workspaceDir(owner))
	if err != nil {
		return delegation.Manifest{}
	}
	defer root.Close()
	return delegation.Snapshot(ctx, root.FS())
}

// askJev asks the decision plugin to pick among the eligible workers or the
// main agent. An empty choice means "use the fallback" with the given reason.
// Its spend is recorded against the delegation like any other usage.
func (g *delegationGrant) askJev(ctx context.Context, d store.Delegation, task delegation.Task, eligible []delegation.Worker) (choice string, confidence float64, model, reason string) {
	s := g.srv
	cfg, changed := s.cfg.DecisionState()
	jev := cfg.Plugins.Jev
	if !jev.Enabled || jev.APIKey == "" || !jev.CanUse(g.owner.Name, g.owner.Role == store.RoleAdmin) {
		return "", 0, "", "jev_unavailable"
	}
	candidates := make([]decision.Candidate, 0, len(eligible)+1)
	for _, w := range eligible {
		candidates = append(candidates, decision.Candidate{Ref: w.Ref, Description: w.Description()})
	}
	owner, _ := s.store.Get(g.session)
	mainModel := ""
	if acct, ok := s.cfg.Account(owner.AccountID); ok {
		mainModel = acct.ResolveModel(owner.DefaultModel)
	}
	p, _, priced := s.cfg.PriceLookup(owner.Agent, mainModel)
	candidates = append(candidates, decision.Candidate{Ref: decision.SelfRef, Description: delegation.SelfDescription(priced, p.Input, p.Output)})
	provider := decision.NewJev(jev)
	var evaluator decision.Provider = provider
	if s.decisionProvider != nil {
		evaluator = s.decisionProvider(jev)
	}
	req := decision.Request{ID: d.ID, TemplateVersion: decision.TemplateVersion, State: delegation.JevState(task), Questions: map[string][]decision.Candidate{"worker": candidates}}
	if provider.ValidateRequest(req) != nil {
		return "", 0, "", "jev_request_limit"
	}
	bound, err := provider.ReservedMicroUSD()
	if err != nil {
		return "", 0, "", "jev_budget_unknown"
	}
	release, err := s.decisionService.Acquire(cfg.Revision, jev.MaxConcurrency)
	if err != nil {
		return "", 0, "", "jev_busy"
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(jev.TimeoutMS)*time.Millisecond)
	defer cancel()
	evaluation, err := invokeDecisionProvider(callCtx, changed, evaluator, req)
	release(evaluation.Dispatched && err != nil && !errors.Is(err, context.Canceled))
	g.recordJevUsage(d, provider.ID(), jev.Model, evaluation, bound)
	if err != nil {
		return "", 0, evaluation.Model, "jev_error"
	}
	suggestion := evaluation.Suggestions["worker"]
	if suggestion.Status != "suggest" {
		return "", suggestion.Confidence, evaluation.Model, "jev_" + suggestion.Reason
	}
	return suggestion.CandidateRef, suggestion.Confidence, evaluation.Model, ""
}

// An unknown cost of a dispatched call is charged at its upper bound: the
// routing decision may not silently become free.
func (g *delegationGrant) recordJevUsage(d store.Delegation, providerID, model string, e decision.Evaluation, bound int64) {
	if !e.Dispatched {
		return
	}
	cost := e.Usage.CostMicroUSD
	if e.Usage.CostStatus != "known" {
		cost = bound
	}
	if e.Model != "" {
		model = e.Model
	}
	price := &store.PriceSnapshot{Version: 1, Source: "table", Key: model, Standard: store.TokenRates{Input: decision.JevInputUSDPerMillion},
		PricingRevision: decision.JevPricingVersion, CatalogVersion: decision.JevPricingVersion, SourceURL: decision.JevPricingSource, VerifiedAt: decision.JevPricingVerifiedAt}
	if providerID == "mindshub" {
		price = &store.PriceSnapshot{Version: 1, Source: "table", Key: model, PricingRevision: decision.MindshubJevPricingVersion,
			CatalogVersion: decision.MindshubJevPricingVersion, SourceURL: decision.MindshubJevPricingSource, VerifiedAt: decision.MindshubJevPricingVerifiedAt}
	}
	raw, _ := json.Marshal(e.Usage)
	ev := store.UsageEvent{TS: time.Now(), User: g.owner.Name, SessionID: g.session, ThreadID: g.thread, TurnID: d.ID, Agent: "decision", Model: model,
		Kind: store.UsageKindDecision, InputTokens: e.Usage.InputTokens, OutputTokens: e.Usage.OutputTokens, CostMicroUSD: cost, Provider: providerID,
		ReqID: "decision:" + d.ID, Raw: string(raw), Price: price, DelegationID: d.ID}
	if err := g.srv.store.InsertUsage(ev); err != nil {
		log.Printf("delegation %s decision usage: %v", d.ID, err)
	}
}

// Compiled adapters are replaceable; a panic is an ambiguous paid attempt,
// never permission to crash the service or dispatch again.
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

// --- worker turn: observation and runtime for chat.Service ---

type delegationTurn struct {
	g                       *delegationGrant
	call                    *delegationCall
	d                       store.Delegation
	owner, exec             store.Session
	same                    bool
	worker                  delegation.Worker
	turnID, pidFile         string
	report                  delegation.Report
	mu                      sync.Mutex
	stop                    func()
	interrupted             atomic.Bool
	terminal, code          string
	failed, ended, ambiguos bool
}

func (t *delegationTurn) Input() store.ChatRequest {
	return store.ChatRequest{RequestID: t.d.ID, SessionID: t.exec.ID, ThreadID: t.g.thread, TurnID: t.turnID, Request: &store.ChatRequestInput{ThreadID: t.g.thread}}
}
func (t *delegationTurn) Advance(string) bool { return true }
func (t *delegationTurn) Fail(err error, code string) {
	t.failed = true
	if code != "" {
		t.code = code
	}
	if err != nil {
		log.Printf("delegation %s worker: %v", t.d.ID, err)
	}
}
func (t *delegationTurn) Terminal(state string) {
	if state == "" {
		return
	}
	if t.terminal != "" && t.terminal != state {
		t.ambiguos = true
	}
	t.terminal = state
}
func (t *delegationTurn) Completed(ctx context.Context) {
	t.ended = ctx.Err() == nil && !t.g.srv.isStopping()
}
func (t *delegationTurn) Finish() {
	if recover() != nil {
		t.failed, t.code = true, "execution_incomplete"
	}
}
func (t *delegationTurn) Interrupted() bool { return t.interrupted.Load() }
func (t *delegationTurn) HistoryFailed()    {}
func (t *delegationTurn) UsageFailed() {
	if t.code == "" {
		t.code = "usage_incomplete"
	}
}
func (t *delegationTurn) Succeeded() bool {
	return !t.failed && t.ended && !t.ambiguos && t.terminal == "completed"
}

func (t *delegationTurn) setStop(stop func()) {
	t.mu.Lock()
	t.stop = stop
	t.mu.Unlock()
	if stop != nil && t.interrupted.Load() {
		stop()
	}
}

// interrupt asks the worker CLI to stop the way the chat room interrupts the
// main agent, then cancels its stream if it has not ended shortly after.
func (t *delegationTurn) interrupt() {
	if t.interrupted.Swap(true) {
		return
	}
	t.mu.Lock()
	stop := t.stop
	t.mu.Unlock()
	if stop != nil {
		stop()
	} else if sess, ok := t.g.srv.store.Get(t.exec.ID); ok && sess.ContainerID != "" {
		// Callers may hold the chat room lock; never wait on Docker here.
		go func() {
			if cmd, err := agent.InterruptCommandFor(t.pidFile); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = t.g.srv.dock.ExecFireAndForget(ctx, sess.ContainerID, cmd)
			}
		}()
	}
	time.AfterFunc(8*time.Second, t.call.cancel)
}

type delegationRuntime struct{ t *delegationTurn }

func (r delegationRuntime) Hold() func() {
	a := r.t.g.srv.workspaces().Activity()
	a.Hold(r.t.exec.ID)
	return func() { a.Release(r.t.exec.ID) }
}
func (r delegationRuntime) Session() (store.Session, bool)      { return r.t.g.srv.store.Get(r.t.exec.ID) }
func (r delegationRuntime) Thread(store.Session) (string, bool) { return r.t.g.thread, false }
func (r delegationRuntime) Options(ctx context.Context, sess store.Session, model, effort, control string, started bool) (agent.TurnOptions, error) {
	t := r.t
	s := t.g.srv
	if u, ok := s.store.GetUser(t.g.owner.Name); !ok || !u.CreatedAt.Equal(t.g.owner.CreatedAt) {
		return agent.TurnOptions{}, errAccountAccess
	}
	if s.quotaBlock(t.g.owner.Name) != "" {
		return agent.TurnOptions{}, chat.ErrOptions
	}
	acct, err := s.sessionAccount(sess)
	if err != nil {
		return agent.TurnOptions{}, err
	}
	if acct.ID != t.worker.AccountID || acct.Type != t.worker.Agent || model != t.worker.Model || !acct.AllowsModel(model) || acct.DelegationTags(model) == nil {
		return agent.TurnOptions{}, chat.ErrOptions
	}
	opts, err := agent.ResolveTurnOptions(sess.Agent, model, effort, control, s.cfg.ConfiguredReasoning(acct, model))
	if err != nil {
		return opts, fmt.Errorf("%w: %v", chat.ErrOptions, err)
	}
	return opts, nil
}
func (r delegationRuntime) Start(ctx context.Context, sess store.Session) (store.Session, error) {
	return r.t.g.srv.workspaces().Start(ctx, sess.ID)
}
func (r delegationRuntime) Attachments(store.Session, []string) error { return nil }

// Record keeps the worker's own transcript beside the owner's chats (outside
// the container mounts) and streams it to the turn's viewers.
func (r delegationRuntime) Record(e chat.Entry, kind, _ string) error {
	t := r.t
	e.TS = time.Now().UTC()
	if e.Kind == "event" {
		t.report.Observe(e.Event)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	t.g.room.broadcast(map[string]any{"type": "delegation_event", "id": t.d.ID, "entry": e})
	root, err := t.g.srv.openDataDir(t.g.srv.sessionDir(t.owner))
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.MkdirAll("delegations", 0o700); err != nil {
		return err
	}
	return root.AppendFile(filepath.Join("delegations", t.d.ID+".jsonl"), append(raw, '\n'), delegationLogLimit)
}
func (r delegationRuntime) Emit(value any) {
	if m, ok := value.(map[string]any); ok && m["type"] == "agent_event" {
		r.t.g.room.broadcast(map[string]any{"type": "delegation_stream", "id": r.t.d.ID, "event": m["event"]})
	}
}
func (r delegationRuntime) Problem(ctx context.Context, operation string, err error, fallback string) protocol.APIProblem {
	return operationProblem(ctx, operation, classifyProblem(err, fallback))
}
func (r delegationRuntime) ProviderSession(_ store.Session, id string) error {
	return r.t.g.srv.store.SaveDelegationProviderSession(r.t.d.ID, id)
}
func (r delegationRuntime) Executor(sess store.Session) chat.Executor {
	t := r.t
	return chat.Executor{Backend: t.g.srv.dock, SetStop: t.setStop, Interrupted: t.Interrupted, Log: log.Printf, Environment: func() ([]string, error) {
		if t.g.ctx.Err() != nil {
			return nil, context.Canceled
		}
		if _, err := r.Options(context.Background(), sess, t.worker.Model, "", "", false); err != nil {
			return nil, err
		}
		return t.g.srv.execEnv(sess)
	}}
}
func (r delegationRuntime) Permission() string                                    { return r.t.g.srv.cfg.GetPermissionMode() }
func (r delegationRuntime) PromptSuggestions(context.Context, store.Session) bool { return false }

// Workers never ask the user: nobody watches their turn.
func (r delegationRuntime) Questions(context.Context, store.Session) bool { return false }
func (r delegationRuntime) Question(agent.Question) (<-chan agent.Answer, func()) {
	return nil, func() {}
}
func (r delegationRuntime) PublishCost(string, string) {}
func (r delegationRuntime) Title(string, string)       {}
