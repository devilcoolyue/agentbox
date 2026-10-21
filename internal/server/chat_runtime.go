package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/protocol"
	"agentbox/internal/store"
)

// chatRuntime adapts a room's persistence and lifecycle to the chat service.
// HTTP authorization and persistent acceptance still precede room ownership.
type chatRuntime struct{ room *chatRoom }

func (a chatRuntime) Hold() func() {
	activity := a.room.srv.workspaces().Activity()
	activity.Hold(a.room.sessID)
	return func() { activity.Release(a.room.sessID) }
}
func (a chatRuntime) Session() (store.Session, bool)        { return a.room.srv.store.Get(a.room.sessID) }
func (a chatRuntime) Thread(s store.Session) (string, bool) { return a.room.preTurnTitleState(s) }
func (a chatRuntime) Options(ctx context.Context, sess store.Session, model, effort, control string, started bool) (agent.TurnOptions, error) {
	s := a.room.srv
	acct, err := s.sessionAccount(sess)
	if err != nil {
		return agent.TurnOptions{}, err
	}
	if !acct.AllowsModel(model) {
		// A turn without an explicit model inherits the workspace default, which
		// the account's list may no longer offer; anything else was chosen.
		if model != sess.DefaultModel {
			return agent.TurnOptions{Model: model}, fmt.Errorf("%w: 模型 %s 不在账号 %s 的可用模型中", chat.ErrOptions, model, acct.Label)
		}
		model = acct.ResolveModel(model)
	}
	capability := s.cfg.ConfiguredReasoning(acct, model)
	if started && capability == nil && effort != "" {
		discovered, _ := s.discoverReasoning(ctx, sess, acct)
		if value, ok := discovered[model]; ok {
			capability = &value
		}
	}
	options, err := agent.ResolveTurnOptions(sess.Agent, model, effort, control, capability)
	if err != nil {
		return options, fmt.Errorf("%w: %v", chat.ErrOptions, err)
	}
	if !started {
		return options, nil
	}
	if err := agent.CheckReasoningInheritance(sess.Agent, s.homeDir(sess), s.workspaceDir(sess), acct.Env, options); err != nil {
		return options, fmt.Errorf("%w: %v", chat.ErrOptions, err)
	}
	if sess.Agent == config.AgentClaude && effort != "" && options.Control == "effort" {
		help, err := a.room.cliHelp(ctx, sess)
		if err != nil || !strings.Contains(help, "--effort") {
			return options, chat.ErrOptions
		}
	}
	return options, nil
}
func (a chatRuntime) PromptSuggestions(ctx context.Context, sess store.Session) bool {
	if sess.Agent != config.AgentClaude {
		return false
	}
	help, err := a.room.cliHelp(ctx, sess)
	return err == nil && strings.Contains(help, "--prompt-suggestions")
}

// Questions needs the CLI's stdin kept open as stream-json and its permission
// prompts routed to the host; AskUserQuestion is only offered then.
func (a chatRuntime) Questions(ctx context.Context, sess store.Session) bool {
	if sess.Agent != config.AgentClaude {
		return false
	}
	help, err := a.room.cliHelp(ctx, sess)
	return err == nil && strings.Contains(help, "--permission-prompt-tool") && strings.Contains(help, "--input-format") && strings.Contains(help, "--replay-user-messages")
}
func (a chatRuntime) Question(q agent.Question) (<-chan agent.Answer, func()) {
	return a.room.offerQuestion(q)
}

// cliHelp caches the Claude CLI's --help per container: the image is fixed at
// creation and the CLI never updates itself inside it (DISABLE_AUTOUPDATER).
func (r *chatRoom) cliHelp(ctx context.Context, sess store.Session) (string, error) {
	r.helpMu.Lock()
	defer r.helpMu.Unlock()
	if r.helpContainer == sess.ContainerID && r.help != "" {
		return r.help, nil
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	help, err := r.srv.dock.ExecCapture(probe, sess.ContainerID, []string{"claude", "--help"}, nil, "")
	if err != nil {
		return "", err
	}
	r.helpContainer, r.help = sess.ContainerID, help
	return help, nil
}
func (a chatRuntime) Start(ctx context.Context, sess store.Session) (store.Session, error) {
	s := a.room.srv
	if sess.Status != store.StatusRunning {
		// The user's message is recorded only once the container is up;
		// until then the page says the workspace is starting.
		a.room.setPhase(chatPhaseStarting)
		defer a.room.setPhase("")
	}
	sess, err := s.startSession(ctx, sess)
	if err != nil {
		return sess, err
	}
	if sess.Agent == config.AgentClaude {
		if err := s.workspaces().UseRunning(ctx, sess.ID, func(current store.Session) error { return s.syncMCP(ctx, current) }); err != nil {
			return sess, &chat.Failure{Code: "mcp_configuration_failed", Cause: err}
		}
	}
	return sess, nil
}
func (a chatRuntime) Attachments(sess store.Session, paths []string) error {
	s := a.room.srv
	s.attachmentMu.Lock()
	defer s.attachmentMu.Unlock()
	valid, err := s.validateChatAttachments(sess, paths)
	if err != nil {
		return err
	}
	for _, ok := range valid {
		if !ok {
			return errors.New("invalid chat attachment")
		}
	}
	return nil
}
func (a chatRuntime) Record(e chat.Entry, kind, retry string) error {
	return a.room.recordChat(e, kind, retry)
}
func (a chatRuntime) Emit(value any) { a.room.broadcast(value) }
func (a chatRuntime) Problem(ctx context.Context, operation string, err error, fallback string) protocol.APIProblem {
	return operationProblem(ctx, operation, classifyProblem(err, fallback))
}
func (a chatRuntime) ProviderSession(sess store.Session, id string) error {
	_, writeErr := a.room.srv.store.Update(sess.ID, func(s *store.Session) { s.ChatSession = id })
	if writeErr != nil {
		log.Printf("save chat session id %s: %v", sess.ID, writeErr)
	}
	return errors.Join(writeErr, a.room.appendLog(logEntry{Kind: "chat_session", Text: id}))
}
func (a chatRuntime) Executor(sess store.Session) chat.Executor {
	r := a.room
	return chat.Executor{Backend: r.srv.dock, Environment: func() ([]string, error) { return r.srv.execEnv(sess) }, SetStop: r.setStop, SetSteer: r.setSteer,
		Interrupted: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.userInterrupted }, Log: log.Printf,
		Signal: func() {
			ctx, cancel := context.WithTimeout(r.srv.workContext(), 10*time.Second)
			defer cancel()
			if err := r.srv.dock.ExecFireAndForget(ctx, sess.ContainerID, agent.InterruptCommand()); err != nil {
				log.Printf("interrupt fallback %s: %v", r.sessID, err)
			}
		}}
}
func (a chatRuntime) Permission() string              { return a.room.srv.cfg.GetPermissionMode() }
func (a chatRuntime) PublishCost(thread, turn string) { a.room.publishTurnCost(thread, turn) }
func (a chatRuntime) TurnEnded(o chat.TurnOutcome) {
	a.room.srv.emitTurnEnded(o)
	a.room.srv.noteChatCache(o)
}
func (a chatRuntime) Title(thread, text string) {
	a.room.srv.spawn(func() { a.room.generateTitle(thread, text) })
}
