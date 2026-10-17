package server

import (
	"log"

	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/pkg/extspi"
)

// Event hooks: the server reports what happened to the extension host, which
// fans it out to the extensions subscribed and bound to it. Nothing here waits
// for an extension.

func workspaceRef(sess store.Session) extspi.WorkspaceRef {
	return extspi.WorkspaceRef{ID: sess.ID, Name: sess.Name, Agent: sess.Agent, Account: sess.AccountID, User: sess.User}
}

func bindingTarget(sess store.Session) config.BindingTarget {
	return config.BindingTarget{Account: sess.AccountID, User: sess.User}
}

// emitWorkspaceLifecycle is the workspace service's lifecycle hook.
func (s *Server) emitWorkspaceLifecycle(action, reason string, sess store.Session) {
	if s.extensions == nil {
		return
	}
	s.extensions.Emit(extspi.HookWorkspaceLifecycle, bindingTarget(sess),
		extspi.EventParams{Workspace: &extspi.WorkspaceEvent{Action: action, Reason: reason, Workspace: workspaceRef(sess)}})
}

// emitTurnEnded reports a finished web chat turn with the cost booked for it.
func (s *Server) emitTurnEnded(o chat.TurnOutcome) {
	if s.extensions == nil || !s.extensions.Uses(extspi.HookChatTurnEnded) {
		return // skip the lookups when nobody listens
	}
	sess, ok := s.store.Get(o.SessionID)
	if !ok {
		return
	}
	turn := &extspi.TurnEvent{Workspace: workspaceRef(sess), Thread: o.ThreadID, Turn: o.TurnID, Status: o.Status, Code: o.Code,
		Model: o.Model, DurationMS: o.Duration.Milliseconds()}
	if o.TurnID != "" && o.ThreadID != "" {
		costs, err := s.store.ChatTurnCosts(sess.ID, o.ThreadID, []string{o.TurnID})
		if err != nil {
			log.Printf("extension event: turn cost %s: %v", sess.ID, err)
		} else if c, ok := costs[o.TurnID]; ok {
			turn.CostMicroUSD, turn.CostSource = c.CostMicroUSD, c.Source
		}
	}
	s.extensions.Emit(extspi.HookChatTurnEnded, bindingTarget(sess), extspi.EventParams{Turn: turn})
}
