package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"

	"agentbox/internal/agent"
	"agentbox/internal/protocol"
	"agentbox/internal/store"
)

// Follow-ups: a message sent while a turn runs either queues (runs as the
// next turn, in order, persisted across restarts) or steers (handed to the
// running interactive Claude turn, which applies it after the current tool
// step). Steering falls back to the queue whenever the turn takes no more
// input: a one-shot CLI, Codex, or a turn whose result already arrived.

// admitChat checks a request against the workspace as it is now and calls
// commit while the active thread and the referenced attachments are pinned,
// so TTL cleanup cannot remove a file between the check and the receipt.
func (s *Server) admitChat(ctx context.Context, room *chatRoom, sessID string, u store.User, input store.ChatRequestInput, commit func() error) error {
	return s.workspaces().WithSession(ctx, sessID, func(current store.Session) error {
		if current.User != u.Name {
			return errSessionGone
		}
		if s.quotaBlock(current.User) != "" {
			return chatRequestProblem("quota_exhausted")
		}
		acct, err := s.sessionAccount(current)
		if err != nil {
			return err
		}
		// Require a concrete model in this protocol, frozen by the client before
		// any asynchronous attachment check; do not reinterpret an empty default.
		if input.Model == "" || !acct.AllowsModel(input.Model) {
			return chatRequestProblem("chat_options_invalid")
		}
		if _, err = agent.ResolveTurnOptions(current.Agent, input.Model, input.Effort, input.EffortControl, s.cfg.ConfiguredReasoning(acct, input.Model)); err != nil {
			return chatRequestProblem("chat_options_invalid")
		}
		room.fileMu.Lock()
		defer room.fileMu.Unlock()
		if err = s.migrateThreads(current); err != nil {
			return err
		}
		// Any thread of the workspace may run, the page's own included when
		// another page made a different one active; a deleted one may not.
		if !s.threadExists(current, input.ThreadID) {
			return chatRequestProblem("chat_thread_changed")
		}
		s.attachmentMu.Lock()
		defer s.attachmentMu.Unlock()
		valid, err := s.validateChatAttachments(current, input.Attachments)
		if err != nil {
			return err
		}
		for _, ok := range valid {
			if !ok {
				return chatRequestProblem("chat_attachments_invalid")
			}
		}
		return commit()
	})
}

// startReceipt hands an accepted receipt to its thread's turn. The caller
// holds the thread (begin) and submitMu; on error the receipt is already
// failed and the caller releases the thread.
func (t *chatTurn) startReceipt(u store.User, receipt store.ChatRequest) error {
	r := t.room
	r.mu.Lock()
	t.requestID = receipt.RequestID
	r.mu.Unlock()
	ctx := operationContext(r.srv.workContext())
	observer := &chatReceiptTurn{turn: t, owner: u, receipt: receipt, outcome: store.ChatUncertain, code: "execution_incomplete"}
	if r.srv.spawn(func() { t.runReceiptTurn(ctx, observer) }) {
		return nil
	}
	failed, err := r.srv.store.AdvanceChatRequest(u, receipt.SessionID, receipt.RequestID, receipt.Revision, store.ChatFailed, "server_stopping")
	if err != nil {
		return err
	}
	r.broadcast(protocol.Event(failed))
	return chatRequestProblem("server_stopping")
}

// kickQueue looks for queued work after anything that may free the room.
func (r *chatRoom) kickQueue() {
	if r.srv != nil && r.srv.store != nil {
		r.srv.spawn(r.dispatchQueue)
	}
}

// dispatchQueue starts queued requests: each thread's oldest, once that thread
// is free and no unreviewed turn blocks it, while the workspace has room for
// another running thread. Threads are tried in the order their requests
// waited. Turn ends, reviews and startup call it; a request the workspace no
// longer admits (thread deleted, quota, account) fails without running and
// the thread's next one is tried. A five-hour pause holds every queue; the
// window's reset dispatches them (quota_pause.go).
func (r *chatRoom) dispatchQueue() {
	s := r.srv
	r.submitMu.Lock()
	defer r.submitMu.Unlock()
	threads, err := s.store.QueuedChatThreads(r.sessID)
	if err != nil {
		log.Printf("chat queue %s: %v", r.sessID, err)
		return
	}
	for _, thread := range threads {
		if r.dispatchThread(thread) == errChatParallel {
			return // every slot is taken; the next turn to end dispatches again
		}
	}
}

// dispatchThread starts the thread's oldest queued request; the caller holds
// submitMu. errChatParallel reports the workspace full.
func (r *chatRoom) dispatchThread(thread string) error {
	s := r.srv
	t := r.turn(thread)
	for {
		c, u, err := s.store.NextQueuedChatRequest(r.sessID, thread)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("chat queue %s: %v", r.sessID, err)
			}
			return nil
		}
		if _, paused := s.quotaPaused(r.sessID); paused {
			return nil
		}
		if err := t.begin(); err != nil {
			return err // running or awaiting review; its end or review dispatches again
		}
		var next store.ChatRequest
		err = s.admitChat(operationContext(s.workContext()), r, r.sessID, u, *c.Request, func() error {
			var err error
			next, err = s.store.AdvanceChatRequest(u, r.sessID, c.RequestID, c.Revision, store.ChatAccepted, "")
			return err
		})
		if err != nil {
			t.release()
			if errors.Is(err, store.ErrChatConflict) {
				continue
			}
			failed, ferr := s.store.AdvanceChatRequest(u, r.sessID, c.RequestID, c.Revision, store.ChatFailed, chatRequestErrorCode(err))
			if ferr != nil {
				log.Printf("chat queue %s request_id=%s: %v", r.sessID, c.RequestID, ferr)
				return nil
			}
			r.broadcast(protocol.Event(failed))
			continue
		}
		r.broadcast(protocol.Event(next))
		if t.startReceipt(u, next) != nil {
			t.release()
		}
		return nil
	}
}

// dispatchQueuedChats resumes queues left by a restart.
func (s *Server) dispatchQueuedChats() {
	sessions, err := s.store.QueuedChatSessions()
	if err != nil {
		log.Printf("chat queue recovery: %v", err)
		return
	}
	for _, id := range sessions {
		s.chat.room(id).dispatchQueue()
	}
}

// queueWaiting reports earlier follow-ups of the thread, which a later one
// must not overtake.
func (s *Server) queueWaiting(session, thread string) bool {
	_, _, err := s.store.NextQueuedChatRequest(session, thread)
	return err == nil
}

func (s *Server) queueChatRequest(w http.ResponseWriter, r *http.Request, room *chatRoom, sess store.Session, u store.User, id string, input store.ChatRequestInput) {
	var receipt store.ChatRequest
	err := s.admitChat(r.Context(), room, sess.ID, u, input, func() error {
		c, fresh, err := s.store.QueueChatRequest(u, sess.ID, id, input)
		if err == nil && !fresh {
			err = store.ErrChatConflict
		}
		receipt = c
		return err
	})
	if err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	room.broadcast(protocol.Event(receipt))
	room.kickQueue() // the turn may have ended between the busy check and this commit
	writeJSON(w, 202, protocol.Response(receipt, false))
}

// steerRequest hands a follow-up to the thread's running interactive turn.
// false means the turn cannot take it and the caller queues it instead. The
// receipt is committed while the turn's input is held open, then written to it.
func (t *chatTurn) steerRequest(ctx context.Context, u store.User, sessID, id string, input store.ChatRequestInput) (store.ChatRequest, bool, error) {
	r := t.room
	r.mu.Lock()
	steer, into := t.steer, t.requestID
	r.mu.Unlock()
	if steer == nil || into == "" {
		return store.ChatRequest{}, false, nil
	}
	var receipt store.ChatRequest
	taken, err := steer(id, input.Text, func() error {
		return r.srv.admitChat(ctx, r, sessID, u, input, func() error {
			c, fresh, err := r.srv.store.SteerChatRequest(u, sessID, id, input, into)
			if err == nil && !fresh {
				err = store.ErrChatConflict
			}
			receipt = c
			return err
		})
	})
	return receipt, taken && err == nil, err
}
