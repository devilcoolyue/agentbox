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

// chatTurnFailed marks a turn that ran and then ended with an error the CLI
// itself reported. It does not block the next message: the process is gone,
// so only its partial file changes are left for the user to look at.
const chatTurnFailed = "turn_failed"

func (t *chatReceiptTurn) Fail(err error, code string) {
	t.code = code
	var exited *chat.ExitError
	switch {
	case t.receipt.State == store.ChatAccepted || t.receipt.State == store.ChatStarting:
		t.outcome = store.ChatFailed
	case t.room.srv.isStopping():
		// Shutdown is not the user confirming an interrupt nor a CLI error.
		t.outcome = store.ChatUncertain
	case (errors.Is(err, errChatInterrupted) || errors.As(err, &exited)) && t.Interrupted():
		t.outcome, t.code = store.ChatInterrupted, "operation_cancelled"
	case errors.As(err, &exited):
		t.outcome, t.code = store.ChatFailed, chatTurnFailed
	default:
		// A lost stream or exit status: the CLI may still be running.
		t.outcome = store.ChatUncertain
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
		// The executor returned, so the provider-reported failure is final. A
		// protocol interrupt ends the CLI's turn as a failed result too.
		if t.Interrupted() {
			t.outcome, t.code = store.ChatInterrupted, "operation_cancelled"
		} else {
			t.outcome, t.code = store.ChatFailed, chatTurnFailed
		}
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
	if t.outcome == store.ChatCompleted || t.outcome == store.ChatInterrupted || t.code == chatTurnFailed {
		if err := t.syncHistory(); err != nil {
			log.Printf("chat receipt history sync failed request_id=%s: %v", t.receipt.RequestID, err)
			t.outcome, t.code = store.ChatUncertain, "history_write_failed"
		}
	}
	if t.outcome == store.ChatUncertain {
		// The code alone is what the user sees; keep the reason findable.
		log.Printf("chat receipt uncertain request_id=%s code=%s", t.receipt.RequestID, t.code)
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
