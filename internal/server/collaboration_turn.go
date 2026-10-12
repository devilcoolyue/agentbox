package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/chat"
	"agentbox/internal/collaboration"
	"agentbox/internal/dockerx"
	"agentbox/internal/protocol"
	"agentbox/internal/store"
)

type collaborationTurn struct {
	srv                                       *Server
	owner                                     store.User
	work                                      store.CollaborationWork
	profile                                   dockerx.CollaborationProfile
	final                                     *agent.FinalOutput
	mu                                        sync.Mutex
	stop                                      func()
	interrupted                               atomic.Bool
	terminal                                  string
	failed, historyFailed, usageFailed, ended bool
	code                                      string
	cause                                     error
	emit                                      func(any)
}

// runCollaborationTurn observes the existing chat service's transcript and
// accounting lifecycle. It never creates a second model/settlement path. The
// caller owns the task admission and completes handoff only after stop/snapshot.
func (s *Server) runCollaborationTurn(ctx context.Context, u store.User, work store.CollaborationWork, p dockerx.CollaborationProfile, emit func(any)) *collaborationTurn {
	cfg, configErr := s.store.CollaborationConfig(work.Run.ID, work.Step.ConfigVersion)
	remaining, timeErr := s.store.CollaborationRuntimeRemaining(u, work.Run.ID)
	if configErr != nil || timeErr != nil {
		return &collaborationTurn{failed: true, work: work, code: "execution_failed", cause: errors.Join(configErr, timeErr)}
	}
	ctx, cancel := context.WithTimeout(ctx, min(cfg.StepTimeout(), remaining))
	defer cancel()
	t := &collaborationTurn{srv: s, owner: u, work: work, profile: p, final: agent.NewFinalOutput(work.Session.Agent, work.Session.ChatSession, collaboration.MaxHandoffBytes), emit: emit}
	jobs := &s.collaborationJobs
	jobs.mu.Lock()
	if jobs.interrupts == nil {
		jobs.interrupts = map[string]func(){}
	}
	jobs.interrupts[work.Run.ID] = t.interrupt
	jobs.mu.Unlock()
	defer func() { jobs.mu.Lock(); delete(jobs.interrupts, work.Run.ID); jobs.mu.Unlock() }()
	// The goroutine is joined before this admitted task returns. It observes
	// only immutable IDs and atomic cancellation; it never mutates turn receipts.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				current, err := s.store.Collaboration(u, work.Run.ID)
				if err != nil || current.State == store.CollabCancelling || current.State == store.CollabDeleted || current.ConfigVersion != work.Step.ConfigVersion {
					t.interrupt()
					grace := time.NewTimer(5 * time.Second)
					defer grace.Stop()
					select {
					case <-ctx.Done():
					case <-grace.C:
						cancel()
					}
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	service := chat.Service{Runtime: collaborationChatRuntime{t}, Usage: s.usageService()}
	b := work.Binding
	service.Run(ctx, chat.Input{SessionID: work.Session.ID, Text: work.Input.Text, Model: b.Model, Effort: b.Effort, Control: b.EffortControl, StrictResume: true, RequireAppServer: true, RequireUsage: true}, t)
	return t
}

func (t *collaborationTurn) Input() store.ChatRequest {
	return store.ChatRequest{RequestID: t.work.Step.ID, SessionID: t.work.Session.ID, ThreadID: t.work.Run.ID, TurnID: t.work.Step.TurnID, Request: &store.ChatRequestInput{Text: t.work.Input.Text, ThreadID: t.work.Run.ID, Model: t.work.Binding.Model, Effort: t.work.Binding.Effort, EffortControl: t.work.Binding.EffortControl}}
}
func (t *collaborationTurn) Advance(state string) bool {
	if state == store.ChatStarting {
		return t.work.Step.State == "starting"
	}
	if state != store.ChatRunning {
		t.failed = true
		return false
	}
	step, err := t.srv.store.AdvanceCollaborationStep(t.owner, t.work.Run.ID, t.work.Step.ID, t.work.Step.Revision, "running")
	if err != nil {
		t.failed = true
		t.code = "execution_failed"
		return false
	}
	t.work.Step = step
	return true
}
func (t *collaborationTurn) Fail(err error, code string) {
	t.cause = err
	t.failed = true
	t.code = "execution_failed"
	if errors.Is(err, agent.ErrResumeUnavailable) {
		t.code = "resume_failed"
	}
}
func (t *collaborationTurn) Terminal(state string) {
	if state == "" {
		return
	}
	if t.terminal != "" && state != t.terminal {
		t.failed = true
	}
	t.terminal = state
}
func (t *collaborationTurn) Interrupted() bool { return t.interrupted.Load() }
func (t *collaborationTurn) HistoryFailed()    { t.historyFailed = true }
func (t *collaborationTurn) UsageFailed()      { t.usageFailed = true }
func (t *collaborationTurn) Succeeded() bool {
	return !t.failed && !t.historyFailed && !t.usageFailed && t.ended && t.terminal == "completed"
}
func (t *collaborationTurn) Completed(ctx context.Context) {
	t.ended = ctx.Err() == nil && !t.srv.isStopping()
}
func (t *collaborationTurn) Finish() {
	if recover() != nil {
		t.failed = true
		t.code = "execution_failed"
	}
	// Usage.Flush is deferred later in chat.Service and has already run here.
	if t.historyFailed || t.syncHistory() != nil {
		t.historyFailed = true
		t.code = "history_write_failed"
	}
	if t.usageFailed {
		t.code = "usage_write_failed"
	}
	if t.terminal != "completed" || !t.ended {
		t.failed = true
	}
}
func (t *collaborationTurn) syncHistory() error {
	root, err := t.srv.openDataDir(filepath.Join(t.srv.sessionDir(t.work.Session), "chats"))
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(t.work.Run.ID + ".jsonl")
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return root.Sync()
}
func (t *collaborationTurn) setStop(stop func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stop = stop
	if stop != nil && t.interrupted.Load() {
		stop()
	}
}
func (t *collaborationTurn) interrupt() {
	if t.interrupted.Swap(true) {
		return
	}
	t.mu.Lock()
	stop := t.stop
	t.mu.Unlock()
	if stop != nil {
		stop()
	}
}

type collaborationChatRuntime struct{ turn *collaborationTurn }

func (r collaborationChatRuntime) Hold() func() {
	a := r.turn.srv.workspaces().Activity()
	a.Hold(r.turn.work.Session.ID)
	return func() { a.Release(r.turn.work.Session.ID) }
}
func (r collaborationChatRuntime) Session() (store.Session, bool) {
	return r.turn.srv.store.Get(r.turn.work.Session.ID)
}
func (r collaborationChatRuntime) Thread(store.Session) (string, bool) {
	return r.turn.work.Run.ID, false
}
func (r collaborationChatRuntime) Options(ctx context.Context, sess store.Session, model, effort, control string, started bool) (agent.TurnOptions, error) {
	t := r.turn
	u, ok := t.srv.store.GetUser(t.owner.Name)
	if !ok || !u.CreatedAt.Equal(t.owner.CreatedAt) {
		return agent.TurnOptions{}, errAccountAccess
	}
	if t.srv.quotaBlock(u.Name) != "" {
		return agent.TurnOptions{}, chat.ErrOptions
	}
	acct, err := t.srv.sessionAccount(sess)
	if err != nil {
		return agent.TurnOptions{}, err
	}
	b := t.work.Binding
	if acct.Type != b.AgentType || acct.ID != b.AccountID || model != b.Model || effort != b.Effort || control != b.EffortControl || !acct.AllowsModel(model) {
		return agent.TurnOptions{}, chat.ErrOptions
	}
	opts, err := agent.ResolveTurnOptions(sess.Agent, model, effort, control, t.srv.cfg.ConfiguredReasoning(acct, model))
	if err != nil {
		return opts, fmt.Errorf("%w: invalid collaboration options", chat.ErrOptions)
	}
	if started {
		if err = agent.CheckReasoningInheritance(sess.Agent, t.profile.Home, t.profile.Workspace, acct.Env, opts); err != nil {
			return opts, chat.ErrOptions
		}
	}
	return opts, nil
}
func (r collaborationChatRuntime) Start(ctx context.Context, sess store.Session) (store.Session, error) {
	t := r.turn
	updated, err := t.srv.workspaces().StartCollaboration(ctx, t.owner, t.work, t.profile)
	if updated.ID != "" {
		t.work.Session = updated
	}
	return updated, err
}
func (r collaborationChatRuntime) Attachments(_ store.Session, paths []string) error {
	if len(paths) > 0 {
		return errors.New("collaboration attachments must use frozen task inputs")
	}
	return nil
}
func (r collaborationChatRuntime) Record(e chat.Entry, kind, retry string) error {
	t := r.turn
	e.TS = time.Now().UTC()
	if e.Kind == "event" {
		t.final.Observe(e.Event)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	root, err := t.srv.openDataDir(filepath.Join(t.srv.sessionDir(t.work.Session), "chats"))
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.AppendFile(t.work.Run.ID+".jsonl", append(raw, '\n'), 128<<20); err != nil {
		return err
	}
	r.Emit(map[string]any{"type": "step_event", "step_id": t.work.Step.ID, "role": t.work.Step.Role, "entry": e})
	return nil
}
func (r collaborationChatRuntime) Emit(value any) {
	if r.turn.emit != nil {
		r.turn.emit(value)
	}
}
func (r collaborationChatRuntime) Problem(ctx context.Context, operation string, err error, fallback string) protocol.APIProblem {
	return operationProblem(ctx, operation, classifyProblem(err, fallback))
}
func (r collaborationChatRuntime) ProviderSession(sess store.Session, id string) error {
	t := r.turn
	return t.srv.store.SaveCollaborationProviderSession(t.owner, t.work.Run.ID, t.work.Step.ID, t.work.Step.Revision, id)
}
func (r collaborationChatRuntime) Executor(sess store.Session) chat.Executor {
	t := r.turn
	return chat.Executor{Backend: t.srv.dock, SetStop: t.setStop, Interrupted: t.Interrupted, Log: log.Printf, Environment: func() ([]string, error) {
		current, err := t.srv.store.CollaborationWork(t.owner, t.work.Run.ID)
		if err != nil {
			return nil, err
		}
		if current.Step.ID != t.work.Step.ID || current.Step.Revision != t.work.Step.Revision || current.Step.State != "running" || current.Run.State == store.CollabCancelling {
			return nil, store.ErrChatConflict
		}
		if _, err = r.Options(context.Background(), sess, t.work.Binding.Model, t.work.Binding.Effort, t.work.Binding.EffortControl, false); err != nil {
			return nil, err
		}
		env, err := t.srv.execEnv(sess)
		if err != nil {
			return nil, err
		}
		return collaborationExecEnv(env)
	}}
}
func (r collaborationChatRuntime) Permission() string              { return r.turn.srv.cfg.GetPermissionMode() }
func (r collaborationChatRuntime) PublishCost(thread, turn string) {}
func (r collaborationChatRuntime) Title(thread, text string)       {} // no hidden model call

func collaborationExecEnv(env []string) ([]string, error) {
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "HOME", "PATH", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "BASH_ENV", "ENV", "NODE_OPTIONS", "LD_PRELOAD", "LD_LIBRARY_PATH", "PYTHONPATH", "PYTHONHOME":
			return nil, errors.New("account environment conflicts with collaboration isolation")
		}
	}
	return env, nil
}
