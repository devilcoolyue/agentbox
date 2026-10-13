package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"agentbox/internal/protocol"
	"agentbox/internal/store"
)

func (s *Server) chatRequestScope(u store.User) string {
	instance, err := s.clientIdentity()
	if err != nil {
		return ""
	}
	return importFingerprint([]string{"chat-requests-v1", instance, u.Name, u.CreatedAt.UTC().Format(time.RFC3339Nano)})
}

type chatRequestProblem string

func (e chatRequestProblem) Error() string { return string(e) }

func chatRequestErrorCode(err error) string {
	var p chatRequestProblem
	switch {
	case errors.As(err, &p):
		return string(p)
	case errors.Is(err, store.ErrChatConflict):
		return "chat_request_conflict"
	case errors.Is(err, store.ErrChatGone):
		return "chat_request_gone"
	case errors.Is(err, store.ErrChatPending):
		return "chat_pending"
	case errors.Is(err, store.ErrChatInvalid):
		return "invalid_request"
	case errors.Is(err, errChatBusy):
		return "chat_busy"
	case errors.Is(err, sql.ErrNoRows):
		return "chat_request_not_found"
	default:
		return classifyProblem(err, "internal_error")
	}
}
func writeChatRequestError(w http.ResponseWriter, r *http.Request, err error) {
	writeProblem(w, r, "chat.request", chatRequestErrorCode(err))
}

func decodeChatRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return store.ErrChatInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return store.ErrChatInvalid
	}
	return nil
}

func canonicalChatInput(in store.ChatRequestInput) (store.ChatRequestInput, error) {
	if !threadIDRe.MatchString(in.ThreadID) {
		return in, store.ErrChatInvalid
	}
	for _, path := range in.Attachments {
		if !draftAttachmentPath.MatchString(path) {
			return in, store.ErrChatInvalid
		}
	}
	if len(in.Attachments) > 64 {
		return in, store.ErrChatInvalid
	}
	return in, nil
}

func (s *Server) handleChatRequestPut(w http.ResponseWriter, r *http.Request, sess store.Session) {
	w.Header().Set("Cache-Control", "no-store")
	id, u := r.PathValue("request"), reqUser(r)
	var input store.ChatRequestInput
	if !operationIDPattern.MatchString(id) || decodeChatRequest(w, r, &input) != nil {
		writeChatRequestError(w, r, store.ErrChatInvalid)
		return
	}
	input, err := canonicalChatInput(input)
	if err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	if scope := s.chatRequestScope(u); scope == "" || input.Scope != scope {
		writeChatRequestError(w, r, chatRequestProblem("chat_scope_changed"))
		return
	}
	room := s.chat.room(sess.ID)
	room.submitMu.Lock()
	defer room.submitMu.Unlock()
	// Replay before quota/account/attachment/thread checks: the original result
	// must stay queryable even when the environment has changed since acceptance.
	if receipt, err := s.store.ReplayChatRequest(u, sess.ID, id, input); !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		receipt, err = room.recoverOrphan(u, receipt)
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		writeJSON(w, 200, protocol.Response(receipt, true))
		return
	}
	if input.Delivery == store.DeliverySteer {
		receipt, steered, err := room.steerRequest(r.Context(), u, sess.ID, id, input)
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		if steered {
			room.broadcast(protocol.Event(receipt))
			writeJSON(w, 202, protocol.Response(receipt, false))
			return
		}
	}
	err = room.begin()
	if err == nil && input.Delivery != "" && s.queueWaiting(sess.ID) {
		room.release() // earlier follow-ups run first
		err = errChatBusy
	}
	if err != nil {
		if input.Delivery != "" && (errors.Is(err, errChatBusy) || errors.Is(err, store.ErrChatPending)) {
			s.queueChatRequest(w, r, room, sess, u, id, input)
			return
		}
		writeChatRequestError(w, r, err)
		return
	}
	scheduled := false
	defer func() {
		if !scheduled {
			room.end()
		}
	}()
	var receipt store.ChatRequest
	err = s.admitChat(r.Context(), room, sess.ID, u, input, func() error {
		var fresh bool
		var err error
		receipt, fresh, err = s.store.AcceptChatRequest(u, sess.ID, id, input)
		if err != nil {
			return err
		}
		if !fresh {
			return store.ErrChatConflict
		} // Only this call's fresh commit schedules.
		return nil
	})
	if err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	if err = room.startReceipt(u, receipt); err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	scheduled = true
	writeJSON(w, 202, protocol.Response(receipt, false))
}

// Caller holds submitMu. Work owns running from before acceptance until after
// accounting/finalization; a persisted active receipt without that owner cannot
// be automatically dispatched. This also handles ambiguous commit errors.
func (room *chatRoom) recoverOrphan(u store.User, c store.ChatRequest) (store.ChatRequest, error) {
	room.mu.Lock()
	defer room.mu.Unlock()
	if room.running || c.State != store.ChatAccepted && c.State != store.ChatStarting && c.State != store.ChatRunning {
		return c, nil
	}
	next, err := room.srv.store.AdvanceChatRequest(u, c.SessionID, c.RequestID, c.Revision, store.ChatUncertain, "execution_incomplete")
	if errors.Is(err, store.ErrChatConflict) {
		return room.srv.store.ChatRequest(u, c.SessionID, c.RequestID)
	}
	return next, err
}

func (s *Server) handleChatRequestGet(w http.ResponseWriter, r *http.Request, sess store.Session) {
	w.Header().Set("Cache-Control", "no-store")
	room := s.chat.room(sess.ID)
	room.submitMu.Lock()
	defer room.submitMu.Unlock()
	c, err := s.store.ChatRequest(reqUser(r), sess.ID, r.PathValue("request"))
	if err == nil {
		c, err = room.recoverOrphan(reqUser(r), c)
	}
	if err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	writeJSON(w, 200, protocol.Response(c))
}

func (s *Server) handleChatRequestList(w http.ResponseWriter, r *http.Request, sess store.Session) {
	w.Header().Set("Cache-Control", "no-store")
	thread := r.URL.Query().Get("thread")
	if thread != "" && !threadIDRe.MatchString(thread) {
		writeChatRequestError(w, r, store.ErrChatInvalid)
		return
	}
	room := s.chat.room(sess.ID)
	room.submitMu.Lock()
	defer room.submitMu.Unlock()
	var pending *store.ChatRequest
	if c, err := s.store.PendingChatRequest(sess.ID); err == nil {
		c, err = s.store.ChatRequest(reqUser(r), sess.ID, c.RequestID)
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		c, err = room.recoverOrphan(reqUser(r), c)
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		if c.State == store.ChatAccepted || c.State == store.ChatStarting || c.State == store.ChatRunning || c.State == store.ChatUncertain {
			pending = &c
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeChatRequestError(w, r, err)
		return
	}
	var rows []store.ChatRequest
	var err error
	if r.URL.Query().Get("pending_only") != "1" {
		rows, err = s.store.RecentChatRequests(reqUser(r), sess.ID, thread)
	} else {
		// Polling pages still need the queue: every page can see and withdraw it.
		rows, err = s.store.QueuedChatRequests(reqUser(r), sess.ID)
	}
	if err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	writeJSON(w, 200, protocol.ChatRequestList{Version: protocol.ChatVersion, Scope: s.chatRequestScope(reqUser(r)), Requests: rows, Pending: pending})
}

func (s *Server) handleChatRequestAction(w http.ResponseWriter, r *http.Request, sess store.Session) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		Action   string `json:"action"`
		Revision int64  `json:"revision"`
		Scope    string `json:"scope"`
	}
	id, u := r.PathValue("request"), reqUser(r)
	if !operationIDPattern.MatchString(id) || decodeChatRequest(w, r, &input) != nil {
		writeChatRequestError(w, r, store.ErrChatInvalid)
		return
	}
	if scope := s.chatRequestScope(u); scope == "" || input.Scope != scope {
		writeChatRequestError(w, r, chatRequestProblem("chat_scope_changed"))
		return
	}
	room := s.chat.room(sess.ID)
	room.submitMu.Lock()
	defer room.submitMu.Unlock()
	if input.Action == "abandon" {
		if err := s.store.AbandonUnknownChatRequest(u, sess.ID, id); err != nil {
			writeChatRequestError(w, r, err)
			return
		}
	} else if input.Action == "withdraw" {
		// A queued follow-up is taken back before it runs; the page restores it.
		c, err := s.store.ChatRequest(u, sess.ID, id)
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		if !(c.State == store.ChatAbandoned && input.Revision == c.Revision-1) {
			if c.State != store.ChatQueued || c.Revision != input.Revision {
				writeChatRequestError(w, r, store.ErrChatConflict)
				return
			}
			if _, err = s.store.AdvanceChatRequest(u, sess.ID, id, c.Revision, store.ChatAbandoned, "withdrawn"); err != nil {
				writeChatRequestError(w, r, err)
				return
			}
		}
	} else if input.Action == "interrupt" {
		c, err := s.store.ChatRequest(u, sess.ID, id)
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		if c.Revision != input.Revision {
			writeChatRequestError(w, r, store.ErrChatConflict)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err = room.interruptRequest(ctx, id, true); err != nil {
			writeChatRequestError(w, r, err)
			return
		}
	} else if input.Action == "review" {
		room.mu.Lock()
		busy := room.running
		room.mu.Unlock()
		if busy {
			writeChatRequestError(w, r, errChatBusy)
			return
		}
		c, err := s.store.ChatRequest(u, sess.ID, id)
		if err != nil {
			writeChatRequestError(w, r, err)
			return
		}
		// Repeated review with the last observed revision is a read-only replay.
		if !(c.State == store.ChatReviewed && (input.Revision == c.Revision || input.Revision == c.Revision-1)) {
			if c.State != store.ChatUncertain || c.Revision != input.Revision {
				writeChatRequestError(w, r, store.ErrChatConflict)
				return
			}
			if _, err = s.store.AdvanceChatRequest(u, sess.ID, id, c.Revision, store.ChatReviewed, ""); err != nil {
				writeChatRequestError(w, r, err)
				return
			}
			room.kickQueue() // follow-ups queued behind the unreviewed turn may run now
		}
	} else {
		writeChatRequestError(w, r, store.ErrChatInvalid)
		return
	}
	c, err := s.store.ChatRequest(u, sess.ID, id)
	if err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	room.broadcast(protocol.Event(c))
	writeJSON(w, 200, protocol.Response(c))
}

func (room *chatRoom) runReceiptTurn(ctx context.Context, turn *chatReceiptTurn) {
	in := turn.receipt.Request
	room.runTurnObserved(ctx, in.Text, in.Model, in.Effort, turn, in.EffortControl)
}
