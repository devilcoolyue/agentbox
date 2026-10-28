package server

// Delegation lets the main agent of an ordinary chat turn hand concrete
// subtasks to cheaper worker models. The main agent (Claude Code) receives a
// stdio MCP tool for exactly one turn; each tool call is routed here over a
// per-turn bridge, routed by operator-supplied model tags (optionally with
// Jev), run as a worker CLI against the same /workspace, settled through the
// ordinary usage path and answered with the worker's report and the files that
// actually changed. The main agent is blocked while a worker runs, so the
// workspace keeps a single writer without copies or snapshots.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/delegation"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

//go:embed delegate_mcp.py
var delegateMCPHelper []byte

const (
	// A delegating turn waits for workers, so it gets at least this long
	// whatever the ordinary turn limit; each worker call is bounded separately.
	delegationTurnMinimum   = 2 * time.Hour
	delegationWorkerTimeout = 20 * time.Minute
	// The MCP wall clock only has to outlast the server's own bounds.
	delegationToolTimeout = 3 * time.Hour
	delegationHelperDir   = ".agentbox"
	delegationHelperFile  = ".agentbox/delegate-mcp.py"
	delegationHelperPath  = "/home/agent/.agentbox/delegate-mcp.py"
	delegationLogLimit    = 32 << 20
)

// The built-in subagent tool ("Agent", formerly "Task") would bypass the
// routing, tags and accounting below, and defaults to the main model.
var delegationDisallowedTools = []string{"Agent", "Task"}

// The delegate tools are the platform's own and need no approval prompt.
var delegationAllowedTools = []string{"mcp__agentbox__delegate", "mcp__agentbox__delegation_feedback"}

type delegationRegistry struct {
	mu     sync.Mutex
	grants map[string]*delegationGrant // owner session → the running turn's grant (one thread at a time)
}

// delegationGrant is the capability one main agent turn holds. It ends with
// the turn: the bridge closes and a running worker is interrupted.
type delegationGrant struct {
	srv      *Server
	chat     *chatTurn // the delegating thread
	owner    store.User
	session  string
	thread   string
	turn     string
	refs     []store.DelegationWorker
	workers  []delegation.Worker
	jev      bool
	ticket   string
	extras   agent.ChatExtras
	ctx      context.Context
	cancel   context.CancelFunc
	deadline time.Time
	shutdown func()

	mu        sync.Mutex
	calls     int
	running   *delegationCall
	closed    bool
	wg        sync.WaitGroup
	closeOnce sync.Once
}

type delegationCall struct {
	id, callID string
	cancel     context.CancelFunc
	mu         sync.Mutex
	interrupt  func()
}

func (c *delegationCall) stop() {
	c.mu.Lock()
	interrupt := c.interrupt
	c.mu.Unlock()
	if interrupt != nil {
		interrupt()
		return
	}
	c.cancel()
}

func newDelegationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// delegationWorkers resolves a roster against the current configuration and
// the owner's access. Refs keep the roster position so a recheck in a later
// call names the same worker the same way.
func (s *Server) delegationWorkers(u store.User, refs []store.DelegationWorker) []delegation.Worker {
	var out []delegation.Worker
	for i, ref := range refs {
		acct, ok := s.cfg.Account(ref.AccountID)
		if !ok || !s.canUseAccount(acct, u.Name) || !acct.AllowsModel(ref.Model) {
			continue
		}
		tags := acct.DelegationTags(ref.Model)
		if tags == nil {
			continue
		}
		w := delegation.Worker{Ref: fmt.Sprintf("w%d", i+1), AccountID: acct.ID, AccountLabel: acct.Label, Agent: acct.Type, Model: ref.Model, ModelLabel: ref.Model, Tags: *tags}
		for _, m := range acct.Models {
			if m.ID == ref.Model && m.Label != "" {
				w.ModelLabel = m.Label
			}
		}
		if p, _, ok := s.cfg.PriceLookup(acct.Type, ref.Model); ok {
			w.Priced, w.PriceIn, w.PriceOut = true, p.Input, p.Output
		}
		out = append(out, w)
	}
	return out
}

// delegationTools is the MCP tool list the helper advertises.
func delegationTools() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []map[string]any{{
		"name": "delegate",
		"description": "Hand one concrete, self-contained subtask to a cheaper worker model that works in the same /workspace. " +
			"The platform chooses the worker or declines (then do the subtask yourself). Blocks until the worker finishes and returns its report, " +
			"the files that actually changed, cost and duration.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task":      str("Self-contained instructions: goal, relevant files, interfaces and conventions, acceptance criteria, what must not be touched."),
				"task_type": map[string]any{"type": "string", "enum": config.DelegationTaskTypes},
				"risks":     map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": config.DelegationRisks}, "description": "Every risk that applies; an empty array if none."},
				"access":    map[string]any{"type": "string", "enum": []string{"read", "write"}},
				"paths":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional paths the subtask is expected to touch."},
				"continue":  str("Optional delegation_id of an earlier call; continues with the same worker and its context, e.g. to apply your review feedback."),
			},
			"required": []string{"task", "task_type", "risks", "access"},
		},
	}, {
		"name":        "delegation_feedback",
		"description": "Record your review of a finished delegation. Used to judge which kinds of subtasks each worker handles well.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"delegation_id": str("The delegation_id returned by delegate."),
				"verdict":       map[string]any{"type": "string", "enum": []string{"ok", "minor_fix", "redo", "abandoned"}},
				"note":          str("Optional short reason."),
			},
			"required": []string{"delegation_id", "verdict"},
		},
	}}
}

// openDelegation prepares the delegate tool for one accepted turn. A nil
// grant without error means the turn runs as an ordinary chat (non-Claude
// workspace or an empty roster after rechecking access and tags).
func (s *Server) openDelegation(turn *chatTurn, u store.User, receipt store.ChatRequest) (*delegationGrant, error) {
	opts := receipt.Request.Delegation
	sess, ok := s.store.Get(turn.room.sessID)
	if !ok {
		return nil, errSessionGone
	}
	workers := s.delegationWorkers(u, opts.Workers)
	if sess.Agent != config.AgentClaude || len(workers) == 0 {
		turn.broadcast(map[string]any{"type": "delegation_notice", "code": "delegation_unavailable_this_turn"})
		return nil, nil
	}
	// Workers share the workspace with the main agent, which waits for them
	// so the files have one writer. One delegating thread per workspace keeps
	// that true for delegation; another thread's message runs as an ordinary
	// turn instead of failing.
	if s.delegationBusy(sess.ID) {
		turn.broadcast(map[string]any{"type": "delegation_notice", "code": "delegation_busy_this_turn"})
		return nil, nil
	}
	jev := false
	if opts.Jev {
		d := s.cfg.GetDecision()
		jev = d.Plugins.Jev.Enabled && d.Plugins.Jev.APIKey != "" && d.Plugins.Jev.CanUse(u.Name, u.Role == store.RoleAdmin)
	}
	pb := s.cfg.GetProxyBridge()
	host, _, err := net.SplitHostPort(pb.Bind)
	if err != nil {
		return nil, fmt.Errorf("delegation bridge address: %w", err)
	}
	root, err := s.openDataDir(s.homeDir(sess))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err = root.MkdirAll(delegationHelperDir, 0o700); err != nil {
		return nil, err
	}
	if err = root.Chown(delegationHelperDir, 1000, 1000); err != nil && !os.IsPermission(err) {
		return nil, err
	}
	if _, err = root.WriteFile(delegationHelperFile, delegateMCPHelper, safefs.WriteOptions{Mode: 0o600, Chown: true, UID: 1000, GID: 1000, BestEffortChown: true}); err != nil {
		return nil, err
	}
	ln, err := s.listenDynamic(host)
	if err != nil {
		return nil, fmt.Errorf("delegation bridge unavailable: %w", err)
	}
	endpoint := "http://" + net.JoinHostPort(pb.Host, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	ctx, cancel := context.WithCancel(s.workContext())
	g := &delegationGrant{srv: s, chat: turn, owner: u, session: sess.ID, thread: receipt.ThreadID, turn: receipt.TurnID,
		refs: opts.Workers, workers: workers, jev: jev, ticket: oauthRandom(), ctx: ctx, cancel: cancel, deadline: time.Now().Add(s.delegationTurnTimeout())}
	tools, _ := json.Marshal(delegationTools())
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"agentbox": map[string]any{
		"type": "stdio", "command": "python3", "args": []string{"-I", delegationHelperPath},
		"env":     map[string]string{"AGENTBOX_DELEGATE_URL": endpoint, "AGENTBOX_DELEGATE_TOKEN": g.ticket, "AGENTBOX_DELEGATE_TOOLS": string(tools)},
		"timeout": delegationToolTimeout.Milliseconds(),
	}}})
	g.extras = agent.ChatExtras{MCPConfig: string(mcp), AppendSystemPrompt: delegation.MainPrompt(workers), AllowedTools: delegationAllowedTools, DisallowedTools: delegationDisallowedTools,
		Env: []string{fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", delegationToolTimeout.Milliseconds())}}
	hs := &http.Server{Handler: s.admit(http.HandlerFunc(g.serve)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 8 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	release, ok := s.track(func() { cancel(); hs.Close(); ln.Close() })
	if !ok {
		return nil, errors.New("server is stopping")
	}
	g.shutdown = func() { cancel(); hs.Close(); ln.Close(); release() }
	if !s.spawn(func() { _ = hs.Serve(ln) }) {
		g.shutdown()
		return nil, errors.New("server is stopping")
	}
	s.delegations.mu.Lock()
	if s.delegations.grants == nil {
		s.delegations.grants = map[string]*delegationGrant{}
	}
	if s.delegations.grants[sess.ID] != nil {
		// Another thread opened its grant while this one was set up.
		s.delegations.mu.Unlock()
		g.shutdown()
		turn.broadcast(map[string]any{"type": "delegation_notice", "code": "delegation_busy_this_turn"})
		return nil, nil
	}
	s.delegations.grants[sess.ID] = g
	s.delegations.mu.Unlock()
	return g, nil
}

// delegationBusy reports whether a thread of the workspace holds a grant.
func (s *Server) delegationBusy(session string) bool {
	s.delegations.mu.Lock()
	defer s.delegations.mu.Unlock()
	return s.delegations.grants[session] != nil
}

// close interrupts a running worker, lets it record its outcome, then tears
// the bridge down. Called once when the main turn ends, before the room opens.
func (g *delegationGrant) close() {
	g.closeOnce.Do(func() {
		g.mu.Lock()
		g.closed = true
		call := g.running
		g.mu.Unlock()
		if call != nil {
			call.stop()
		}
		g.wait(20 * time.Second)
		g.shutdown()
		g.wait(30 * time.Second)
		s := g.srv
		s.delegations.mu.Lock()
		if s.delegations.grants[g.session] == g {
			delete(s.delegations.grants, g.session)
		}
		s.delegations.mu.Unlock()
	})
}

func (g *delegationGrant) wait(limit time.Duration) {
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// interruptActive stops a running worker for a user interrupt of the turn.
// turnTimeout is how long one chat turn may run before it is stopped.
func (s *Server) turnTimeout() time.Duration {
	if s.turnLimit > 0 {
		return s.turnLimit
	}
	return s.cfg.GetTurnTimeout()
}

// delegationTurnTimeout bounds a turn that may delegate: the configured turn
// limit, but never less than delegationTurnMinimum.
func (s *Server) delegationTurnTimeout() time.Duration {
	return max(s.turnTimeout(), delegationTurnMinimum)
}

// interruptDelegation stops the worker of the thread's turn, if it delegates;
// a grant held by another thread of the workspace is left alone.
func (s *Server) interruptDelegation(session, thread string) {
	s.delegations.mu.Lock()
	g := s.delegations.grants[session]
	s.delegations.mu.Unlock()
	if g == nil || g.thread != thread {
		return
	}
	g.mu.Lock()
	call := g.running
	g.mu.Unlock()
	if call != nil {
		call.stop()
	}
}

type delegationRequest struct {
	delegation.Task
	CallID       string `json:"call_id"`
	DelegationID string `json:"delegation_id"`
	Verdict      string `json:"verdict"`
	Note         string `json:"note"`
}

func (g *delegationGrant) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+g.ticket)) != 1 {
		writeErr(w, http.StatusUnauthorized, "delegation grant invalid")
		return
	}
	if g.ctx.Err() != nil {
		writeJSON(w, http.StatusGone, map[string]string{"status": "failed", "error": "this turn has ended"})
		return
	}
	var body delegationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "failed", "error": "invalid JSON arguments"})
		return
	}
	switch r.URL.Path {
	case "/delegate":
		g.delegate(r.Context(), w, body.Task, body.CallID)
	case "/feedback":
		note := delegation.Truncate(body.Note, delegation.MaxNoteBytes)
		if err := g.srv.store.RateDelegation(g.session, g.turn, body.DelegationID, body.Verdict, note); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "failed", "error": "unknown delegation_id for this turn, unfinished delegation, or invalid verdict"})
			return
		}
		if d, err := g.srv.store.SessionDelegation(g.session, body.DelegationID); err == nil {
			g.publish(d, false)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
	case "/cancel":
		g.mu.Lock()
		call := g.running
		g.mu.Unlock()
		if call != nil && call.callID == body.CallID {
			call.stop()
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (g *delegationGrant) delegate(ctx context.Context, w http.ResponseWriter, task delegation.Task, callID string) {
	if err := task.Normalize(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "failed", "error": err.Error()})
		return
	}
	g.mu.Lock()
	switch {
	case g.closed:
		g.mu.Unlock()
		writeJSON(w, http.StatusGone, map[string]string{"status": "failed", "error": "this turn has ended"})
		return
	case g.running != nil:
		g.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"status": "failed", "error": "another delegation is still running; wait for its result"})
		return
	case g.calls >= delegation.MaxCallsPerTurn:
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "declined", "reason": "turn_limit", "advice": fmt.Sprintf("The limit of %d delegations for this turn is reached. Do the remaining work yourself.", delegation.MaxCallsPerTurn)})
		return
	}
	g.calls++
	callCtx, cancel := context.WithCancel(ctx)
	call := &delegationCall{id: newDelegationID(), callID: callID, cancel: cancel}
	g.running = call
	g.wg.Add(1)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.running = nil
		g.mu.Unlock()
		cancel()
		g.wg.Done()
	}()
	writeJSON(w, http.StatusOK, g.run(callCtx, call, task))
}

// delegationResult is what the main agent reads back from the tool.
type delegationResult struct {
	DelegationID string              `json:"delegation_id,omitempty"`
	Status       string              `json:"status"`
	Worker       string              `json:"worker,omitempty"`
	Route        string              `json:"route,omitempty"`
	Reason       string              `json:"reason,omitempty"`
	Advice       string              `json:"advice,omitempty"`
	Report       string              `json:"report,omitempty"`
	Changes      *delegation.Changes `json:"changed_files,omitempty"`
	CostUSD      float64             `json:"cost_usd"`
	DurationS    int64               `json:"duration_s"`
	Error        string              `json:"error,omitempty"`
}

func (g *delegationGrant) run(ctx context.Context, call *delegationCall, task delegation.Task) delegationResult {
	s := g.srv
	started := time.Now()
	d, err := s.store.CreateDelegation(store.Delegation{ID: call.id, User: g.owner.Name, SessionID: g.session, ThreadID: g.thread, TurnID: g.turn,
		TaskType: task.Type, Risks: task.Risks, Access: task.Access, Task: task.Text, ContinueOf: task.Continue})
	if err != nil {
		log.Printf("delegation %s create: %v", g.session, err)
		return delegationResult{Status: "failed", Error: "could not record the delegation"}
	}
	g.publish(d, false)
	route, worker, prev, declined := g.route(ctx, d, task)
	routeJSON, _ := json.Marshal(route)
	if declined {
		d, err = s.store.RouteDelegation(d.ID, routeJSON, store.Delegation{}, true)
		if err != nil {
			return delegationResult{DelegationID: call.id, Status: "failed", Error: "could not record the routing decision"}
		}
		g.publish(d, true)
		return delegationResult{DelegationID: d.ID, Status: "declined", Reason: route.Reason, Route: routeSummary(route),
			Advice: "This subtask should not go to a worker. Do it yourself; do not resubmit it in other words.", CostUSD: float64(s.store.DelegationCost(d.ID)) / 1e6}
	}
	return g.execute(ctx, call, d, route, routeJSON, worker, prev, task, started)
}

func routeSummary(r delegation.Route) string {
	switch r.Source {
	case "jev":
		return fmt.Sprintf("Jev (%s, confidence %.2f)", r.Reason, r.Confidence)
	case "":
		return r.Reason
	}
	return r.Source + ": " + r.Reason
}

// route applies, in order: the continue request, the hard tag filters, Jev
// (when enabled and allowed) choosing among the eligible workers or the main
// agent itself, and the deterministic fallback.
func (g *delegationGrant) route(ctx context.Context, d store.Delegation, task delegation.Task) (delegation.Route, delegation.Worker, store.Delegation, bool) {
	s := g.srv
	workers := s.delegationWorkers(g.owner, g.refs)
	eligible, rejected := delegation.Eligible(workers, task)
	route := delegation.Route{Rejected: rejected, Eligible: []string{}, Roster: workers}
	for _, w := range eligible {
		route.Eligible = append(route.Eligible, w.Ref)
	}
	if s.quotaBlock(g.owner.Name) != "" {
		route.Source, route.Reason = "rules", "quota_exhausted"
		return route, delegation.Worker{}, store.Delegation{}, true
	}
	if task.Continue != "" {
		if prev, err := s.store.SessionDelegation(g.session, task.Continue); err == nil && prev.ProviderSession != "" {
			for _, w := range eligible {
				if w.AccountID == prev.AccountID && w.Model == prev.Model {
					route.Source, route.Reason = "continue", "same_worker"
					return route, w, prev, false
				}
			}
		}
	}
	if len(eligible) == 0 {
		route.Source, route.Reason = "rules", "no_eligible_worker"
		return route, delegation.Worker{}, store.Delegation{}, true
	}
	if g.jev {
		choice, confidence, model, reason := g.askJev(ctx, d, task, eligible)
		route.JevModel = model
		switch {
		case choice == "self":
			route.Source, route.Reason, route.Confidence = "jev", "main_agent", confidence
			return route, delegation.Worker{}, store.Delegation{}, true
		case choice != "":
			for _, w := range eligible {
				if w.Ref == choice {
					route.Source, route.Reason, route.Confidence = "jev", "worker", confidence
					return route, w, store.Delegation{}, false
				}
			}
		}
		route.Source, route.Reason = "fallback", reason
		return route, delegation.Pick(eligible), store.Delegation{}, false
	}
	route.Source = "rules"
	route.Reason = "only_eligible_worker"
	if len(eligible) > 1 {
		route.Reason = "cheapest_eligible_worker"
	}
	return route, delegation.Pick(eligible), store.Delegation{}, false
}

// publish broadcasts a delegation's state to the turn's viewers and, for the
// states that end or start real work, appends it to the thread transcript so
// history can rebuild the card.
func (g *delegationGrant) publish(d store.Delegation, persist bool) {
	view := g.srv.delegationView(d)
	raw, err := json.Marshal(view)
	if err != nil {
		return
	}
	g.chat.broadcast(map[string]any{"type": "delegation", "delegation": view})
	if persist {
		if err := g.chat.room.appendLog(g.thread, logEntry{Kind: "delegation", Event: raw}); err != nil {
			log.Printf("delegation %s history: %v", d.ID, err)
		}
	}
}

// delegationView is the browser-facing shape of a delegation.
type delegationView struct {
	store.Delegation
	WorkerLabel  string  `json:"worker_label,omitempty"`
	AccountLabel string  `json:"account_label,omitempty"`
	CostMicroUSD int64   `json:"cost_micro_usd"`
	DurationMS   int64   `json:"duration_ms,omitempty"`
	RouteText    string  `json:"route_text,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
}

func (s *Server) delegationView(d store.Delegation) delegationView {
	v := delegationView{Delegation: d, CostMicroUSD: s.store.DelegationCost(d.ID)}
	if acct, ok := s.cfg.Account(d.AccountID); ok {
		v.AccountLabel = acct.Label
		v.WorkerLabel = d.Model
		for _, m := range acct.Models {
			if m.ID == d.Model && m.Label != "" {
				v.WorkerLabel = m.Label
			}
		}
	}
	var route delegation.Route
	if len(d.Route) > 0 && json.Unmarshal(d.Route, &route) == nil {
		v.RouteText = route.Source + ":" + route.Reason
		v.Confidence = route.Confidence
		// The roster snapshot is for the record; viewers need only the outcome.
		route.Roster = nil
		v.Route, _ = json.Marshal(route)
	}
	if start, err := time.Parse(time.RFC3339Nano, d.StartedAt); err == nil {
		if end, err := time.Parse(time.RFC3339Nano, d.FinishedAt); err == nil {
			v.DurationMS = end.Sub(start).Milliseconds()
		}
	}
	return v
}
