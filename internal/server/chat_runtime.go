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

// chatRuntime adapts a thread's persistence and lifecycle to the chat service.
// HTTP authorization and persistent acceptance still precede thread ownership.
type chatRuntime struct{ turn *chatTurn }

func (a chatRuntime) room() *chatRoom { return a.turn.room }

func (a chatRuntime) Hold() func() {
	activity := a.room().srv.workspaces().Activity()
	activity.Hold(a.room().sessID)
	return func() { activity.Release(a.room().sessID) }
}
func (a chatRuntime) Session() (store.Session, bool)        { return a.room().srv.store.Get(a.room().sessID) }
func (a chatRuntime) Thread(s store.Session) (string, bool) { return a.turn.preTurnTitleState(s) }

// Conversation keeps the active thread on the session's recorded conversation
// exactly as before; another thread resumes the one its transcript names.
func (a chatRuntime) Conversation(sess store.Session, thread string) string {
	r := a.room()
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if thread == "" || r.srv.activeThread(sess) == thread {
		return sess.ChatSession
	}
	return r.srv.threadResumeID(sess, thread)
}
func (a chatRuntime) Options(ctx context.Context, sess store.Session, model, effort, control string, started bool) (agent.TurnOptions, error) {
	s := a.room().srv
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
		help, err := a.room().cliHelp(ctx, sess)
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
	help, err := a.room().cliHelp(ctx, sess)
	return err == nil && strings.Contains(help, "--prompt-suggestions")
}

// Questions needs the CLI's stdin kept open as stream-json and its permission
// prompts routed to the host; AskUserQuestion is only offered then.
func (a chatRuntime) Questions(ctx context.Context, sess store.Session) bool {
	if sess.Agent != config.AgentClaude {
		return false
	}
	help, err := a.room().cliHelp(ctx, sess)
	return err == nil && strings.Contains(help, "--permission-prompt-tool") && strings.Contains(help, "--input-format") && strings.Contains(help, "--replay-user-messages")
}
func (a chatRuntime) Question(q agent.Question) (<-chan agent.Answer, func()) {
	return a.room().offerQuestion(q)
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
	s := a.room().srv
	if sess.Status != store.StatusRunning {
		// The user's message is recorded only once the container is up;
		// until then the page says the workspace is starting.
		a.turn.setPhase(chatPhaseStarting)
		defer a.turn.setPhase("")
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
	s := a.room().srv
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
	return a.turn.recordChat(e, kind, retry)
}
func (a chatRuntime) Emit(value any) { a.turn.broadcast(value) }
func (a chatRuntime) Problem(ctx context.Context, operation string, err error, fallback string) protocol.APIProblem {
	return a.room().srv.turnProblem(ctx, operation, err, fallback)
}
func (a chatRuntime) Stopping() bool { return a.room().srv.stopping() }
func (a chatRuntime) Interrupt() {
	go func() {
		ctx, cancel := context.WithTimeout(a.room().srv.workContext(), 10*time.Second)
		defer cancel()
		a.turn.interruptContext(ctx)
	}()
}

// ProviderSession records the conversation in the thread's transcript. The
// session keeps the active thread's, so it changes only while this thread is
// the active one; activating a thread restores its own from the transcript.
func (a chatRuntime) ProviderSession(sess store.Session, id string) error {
	r := a.room()
	var writeErr error
	r.fileMu.Lock()
	active := r.srv.activeThread(sess) == a.turn.thread
	if active {
		_, writeErr = r.srv.store.Update(sess.ID, func(s *store.Session) { s.ChatSession = id })
	}
	r.fileMu.Unlock()
	if writeErr != nil {
		log.Printf("save chat session id %s: %v", sess.ID, writeErr)
	}
	return errors.Join(writeErr, r.appendLog(a.turn.thread, logEntry{Kind: "chat_session", Text: id}))
}
func (a chatRuntime) Executor(sess store.Session) chat.Executor {
	t, r := a.turn, a.room()
	return chat.Executor{Backend: r.srv.dock, Environment: func() ([]string, error) { return r.srv.execEnv(sess) }, SetStop: t.setStop, SetSteer: t.setSteer,
		Interrupted: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return t.userInterrupted }, Log: log.Printf,
		Signal: func() {
			ctx, cancel := context.WithTimeout(r.srv.workContext(), 10*time.Second)
			defer cancel()
			cmd, err := agent.InterruptCommandFor(agent.ChatPidFile(t.thread))
			if err == nil {
				err = r.srv.dock.ExecFireAndForget(ctx, sess.ContainerID, cmd)
			}
			if err != nil {
				log.Printf("interrupt fallback %s: %v", r.sessID, err)
			}
		}}
}
func (a chatRuntime) Permission() string { return a.room().srv.cfg.GetPermissionMode() }
func (a chatRuntime) PublishCost(thread, turn string, running bool) {
	a.room().publishTurnCost(thread, turn, running)
}
func (a chatRuntime) TurnEnded(o chat.TurnOutcome) {
	r := a.room()
	r.srv.emitTurnEnded(o)
	r.srv.noteChatCache(o)
	if o.Code == chat.ChatQuotaPaused {
		r.mu.Lock()
		id := a.turn.requestID
		r.mu.Unlock()
		r.srv.spawn(func() { r.queueQuotaResume(id) })
	}
}
func (a chatRuntime) FiveHour(w agent.RateWindow) bool {
	sess, ok := a.room().srv.store.Get(a.room().sessID)
	return ok && a.room().srv.noteFiveHour(sess, w)
}
func (a chatRuntime) Title(thread, text string) {
	r := a.room()
	r.srv.spawn(func() { r.generateTitle(thread, text) })
}
