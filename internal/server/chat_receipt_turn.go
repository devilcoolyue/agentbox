package server

import (
	"context"
	"errors"
	"log"

	"agentbox/internal/chat"
	"agentbox/internal/protocol"
	"agentbox/internal/store"
)

// chatReceiptTurn observes the existing execution/settlement path. It never
// invokes a second runner or reconstructs usage after a lost acknowledgement.
type chatReceiptTurn struct {
	room                        *chatRoom
	owner                       store.User
	receipt                     store.ChatRequest
	outcome, code               string
	terminal                    string
	ambiguous                   bool
	logFailed, settlementFailed bool
}

func (t *chatReceiptTurn) Advance(state string) bool {
	c, err := t.room.srv.store.AdvanceChatRequest(t.owner, t.receipt.SessionID, t.receipt.RequestID, t.receipt.Revision, state, "")
	if err != nil {
		t.outcome, t.code = store.ChatUncertain, "receipt_write_failed"
		return false // Never start external work after a failed stage commit.
	}
	t.receipt = c
	t.room.broadcast(protocol.Event(c))
	return true
}

func (t *chatReceiptTurn) Terminal(state string) {
	if state == "" {
		return
	}
	if t.terminal != "" && t.terminal != state {
		t.ambiguous = true
	}
	t.terminal = state
}

func (t *chatReceiptTurn) Interrupted() bool {
	t.room.mu.Lock()
	defer t.room.mu.Unlock()
	return t.room.userInterrupted
}

func (s *Server) isStopping() bool {
	l := s.runtime()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopping
}

var errChatInterrupted = chat.ErrInterrupted

func (t *chatReceiptTurn) Fail(err error, code string) {
	t.code = code
	if t.receipt.State == store.ChatAccepted || t.receipt.State == store.ChatStarting {
		t.outcome = store.ChatFailed
	} else {
		t.outcome = store.ChatUncertain
		if errors.Is(err, errChatInterrupted) && t.Interrupted() && !t.room.srv.isStopping() {
			t.outcome = store.ChatInterrupted
			t.code = "operation_cancelled"
		}
	}
}

func (t *chatReceiptTurn) Completed(ctx context.Context) {
	// Evaluate cancellation before defer cancel runs. Shutdown is not the user
	// confirming an interrupt, even if its stop signal produced a terminal event.
	if ctx.Err() != nil || t.room.srv.isStopping() || t.ambiguous {
		return
	}
	switch t.terminal {
	case "completed":
		t.outcome, t.code = store.ChatCompleted, ""
	case "interrupted":
		if t.Interrupted() {
			t.outcome, t.code = store.ChatInterrupted, "operation_cancelled"
		}
	case "failed":
		t.outcome, t.code = store.ChatUncertain, "chat_failed"
	}
}

// Registered before usage's defer and after room.end's defer, so persistence
// and accounting finish before the room is released. A panic cannot publish a
// successful receipt; no prompt, raw provider error or panic value is logged.
func (t *chatReceiptTurn) Finish() {
	if recover() != nil {
		t.outcome, t.code = store.ChatUncertain, "execution_incomplete"
	}
	if t.logFailed {
		t.outcome, t.code = store.ChatUncertain, "history_write_failed"
	}
	if t.settlementFailed {
		t.outcome, t.code = store.ChatUncertain, "usage_write_failed"
	}
	if t.outcome == store.ChatCompleted || t.outcome == store.ChatInterrupted {
		if err := t.syncHistory(); err != nil {
			t.outcome, t.code = store.ChatUncertain, "history_write_failed"
		}
	}
	c, err := t.room.srv.store.AdvanceChatRequest(t.owner, t.receipt.SessionID, t.receipt.RequestID, t.receipt.Revision, t.outcome, t.code)
	if err != nil {
		// The persisted active state continues to block admission. Query/restart
		// recovery exposes it as uncertain; a deleted receipt stays a tombstone.
		log.Printf("chat receipt finalization failed request_id=%s", t.receipt.RequestID)
		return
	}
	t.receipt = c
	t.room.broadcast(protocol.Event(c))
}

// A final receipt must not outrun its transcript writes. Sync the file and its
// directory before committing completed/interrupted; failed sync stays unknown.
func (t *chatReceiptTurn) syncHistory() error {
	r := t.room
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok {
		return errSessionGone
	}
	root, err := r.srv.openDataDir(r.srv.chatsDir(sess))
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(t.receipt.ThreadID + ".jsonl")
	if err != nil {
		return err
	}
	err = f.Sync()
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return root.Sync()
}

func (t *chatReceiptTurn) Input() store.ChatRequest { return t.receipt }
func (t *chatReceiptTurn) HistoryFailed()           { t.logFailed = true }
func (t *chatReceiptTurn) UsageFailed()             { t.settlementFailed = true }
func (t *chatReceiptTurn) Succeeded() bool          { return t.outcome == store.ChatCompleted }
