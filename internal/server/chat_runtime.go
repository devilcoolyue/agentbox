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
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		help, err := s.dock.ExecCapture(probe, sess.ContainerID, []string{"claude", "--help"}, nil, "")
		if err != nil || !strings.Contains(help, "--effort") {
			return options, chat.ErrOptions
		}
	}
	return options, nil
}
func (a chatRuntime) Start(ctx context.Context, sess store.Session) (store.Session, error) {
	s := a.room.srv
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
	return chat.Executor{Backend: r.srv.dock, Environment: func() ([]string, error) { return r.srv.execEnv(sess) }, SetStop: r.setStop,
		Interrupted: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.userInterrupted }, Log: log.Printf}
}
func (a chatRuntime) Permission() string              { return a.room.srv.cfg.GetPermissionMode() }
func (a chatRuntime) PublishCost(thread, turn string) { a.room.publishTurnCost(thread, turn) }
func (a chatRuntime) Title(thread, text string) {
	a.room.srv.spawn(func() { a.room.generateTitle(thread, text) })
}
