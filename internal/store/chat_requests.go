package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrChatConflict = errors.New("chat request ID or revision conflicts")
	ErrChatPending  = errors.New("chat request is active or needs review")
	ErrChatGone     = errors.New("chat request was abandoned or deleted")
	ErrChatInvalid  = errors.New("invalid chat request")
)

const (
	// ChatQueued waits for the session's running turn; it holds no active slot.
	ChatQueued      = "queued"
	ChatAccepted    = "accepted"
	ChatStarting    = "starting"
	ChatRunning     = "running"
	ChatCompleted   = "completed"
	ChatFailed      = "failed"
	ChatInterrupted = "interrupted"
	ChatUncertain   = "uncertain"
	ChatReviewed    = "reviewed"
	ChatAbandoned   = "abandoned"
	ChatDeleted     = "deleted"
)

// ChatRequestInput is frozen before acceptance. Scope fences browser copies
// from a different instance/user lifetime; its value is validated by the server.
// Text includes the existing attachment markers used by the runner. Attachment
// references are also saved separately for pre-execution validation.
type ChatRequestInput struct {
	Scope         string   `json:"scope"`
	ThreadID      string   `json:"thread_id"`
	Text          string   `json:"text"`
	Model         string   `json:"model"`
	Effort        string   `json:"effort"`
	EffortControl string   `json:"effort_control"`
	Attachments   []string `json:"attachments,omitempty"`
	// Delegation lets the main agent hand subtasks to the listed worker models
	// during this turn. Absent keeps the turn an ordinary single-agent chat and
	// the fingerprint of requests written before delegation existed.
	Delegation *DelegationOptions `json:"delegation,omitempty"`
	// Suggestions asks for a predicted next prompt after the turn, an extra
	// charged request; false is omitted so older fingerprints stay unchanged.
	Suggestions bool `json:"suggestions,omitempty"`
	// Questions says the sending page can show the model's questions and
	// answer them within the turn; omitted when false, like Suggestions.
	Questions bool `json:"questions,omitempty"`
	// Tasks turns on Claude Code's task list for models it leaves it off for
	// (Claude 5), which can cost more tokens; omitted when false.
	Tasks bool `json:"tasks,omitempty"`
	// Delivery is how a message sent while a turn runs should reach the agent:
	// "queue" runs it as the next turn, "steer" hands it to the running turn
	// when that turn takes more input (else it queues). Empty is an ordinary
	// send, refused while busy, and keeps older fingerprints.
	Delivery string `json:"delivery,omitempty"`
}

const (
	DeliveryQueue = "queue"
	DeliverySteer = "steer"
)

// DelegationOptions is frozen with the receipt: a retry or replay can never
// widen the worker roster or switch Jev on after acceptance. Every worker is
// rechecked against the current account access before each delegate call.
type DelegationOptions struct {
	Version int                `json:"version"`
	Workers []DelegationWorker `json:"workers"`
	Jev     bool               `json:"jev"`
}
type DelegationWorker struct {
	AccountID string `json:"account_id"`
	Model     string `json:"model"`
}

const DelegationVersion = 1
const MaxDelegationWorkers = 8

func (d *DelegationOptions) valid() bool {
	if d == nil {
		return true
	}
	if d.Version != DelegationVersion || len(d.Workers) < 1 || len(d.Workers) > MaxDelegationWorkers {
		return false
	}
	seen := map[DelegationWorker]bool{}
	for _, w := range d.Workers {
		if w.AccountID == "" || w.Model == "" || len(w.AccountID) > 128 || len(w.Model) > 256 || !utf8.ValidString(w.AccountID) || !utf8.ValidString(w.Model) || seen[w] {
			return false
		}
		seen[w] = true
	}
	return true
}

type ChatRequest struct {
	RequestID string            `json:"request_id"`
	SessionID string            `json:"session_id"`
	ThreadID  string            `json:"thread_id"`
	TurnID    string            `json:"turn_id"`
	Request   *ChatRequestInput `json:"request"`
	State     string            `json:"state"`
	ErrorCode string            `json:"error_code,omitempty"`
	Revision  int64             `json:"revision"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	// MergedInto names the running request a steered message was handed to;
	// such a request is completed on delivery and never runs a turn itself.
	MergedInto  string `json:"merged_into,omitempty"`
	Fingerprint string `json:"-"`
}

const chatRequestColumns = "request_id,session_id,thread_id,turn_id,request_json,state,error_code,revision,created_at,updated_at,merged_into,fingerprint"
const chatRequestKey = "user=? AND user_created_at=? AND session_id=? AND request_id=?"
const chatRequestActive = "state IN ('accepted','starting','running','uncertain')"

func scanChatRequest(row interface{ Scan(...any) error }) (ChatRequest, error) {
	var c ChatRequest
	var raw string
	err := row.Scan(&c.RequestID, &c.SessionID, &c.ThreadID, &c.TurnID, &raw, &c.State, &c.ErrorCode, &c.Revision, &c.CreatedAt, &c.UpdatedAt, &c.MergedInto, &c.Fingerprint)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c.Request)
	}
	return c, err
}

func validChatKey(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func encodeChatRequest(in ChatRequestInput) ([]byte, string, error) {
	if !validChatKey(in.Scope) || !validChatKey(in.ThreadID) || strings.TrimSpace(in.Text) == "" || len(in.Text) > 1<<20 || len(in.Attachments) > 64 {
		return nil, "", ErrChatInvalid
	}
	for _, field := range append([]string{in.Model, in.Effort, in.EffortControl}, in.Attachments...) {
		if len(field) > 512 || !utf8.ValidString(field) {
			return nil, "", ErrChatInvalid
		}
	}
	if !utf8.ValidString(in.Text) || !in.Delegation.valid() || in.Delivery != "" && in.Delivery != DeliveryQueue && in.Delivery != DeliverySteer {
		return nil, "", ErrChatInvalid
	}
	raw, err := json.Marshal(in)
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), err
}

// AcceptChatRequest commits the receipt before returning fresh=true. Only that
// caller may schedule work; replay NEVER grants permission to run again. The
// server must additionally serialize with legacy turns/thread changes and
// validate the active thread, scope, attachments, quota and account admission.
func (s *Store) AcceptChatRequest(u User, session, id string, in ChatRequestInput) (ChatRequest, bool, error) {
	return s.acceptChatRequest(u, session, id, in, ChatAccepted, "")
}

// QueueChatRequest accepts a follow-up behind the session's running or
// unreviewed turn. Like AcceptChatRequest, only fresh=true may be dispatched,
// and only by taking it from the queue in order.
func (s *Store) QueueChatRequest(u User, session, id string, in ChatRequestInput) (ChatRequest, bool, error) {
	return s.acceptChatRequest(u, session, id, in, ChatQueued, "")
}

// SteerChatRequest records a follow-up handed to the running request into.
// It is completed on commit: the caller writes it to that turn right after,
// under the lock that keeps the turn's input open.
func (s *Store) SteerChatRequest(u User, session, id string, in ChatRequestInput, into string) (ChatRequest, bool, error) {
	if !validChatKey(into) {
		return ChatRequest{}, false, ErrChatInvalid
	}
	return s.acceptChatRequest(u, session, id, in, ChatCompleted, into)
}

func (s *Store) acceptChatRequest(u User, session, id string, in ChatRequestInput, state, into string) (ChatRequest, bool, error) {
	if !validChatKey(session) || !validChatKey(id) {
		return ChatRequest{}, false, ErrChatInvalid
	}
	raw, fingerprint, err := encodeChatRequest(in)
	if err != nil {
		return ChatRequest{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ChatRequest{}, false, err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return ChatRequest{}, false, err
	}
	c, err := scanChatRequest(tx.QueryRow("SELECT "+chatRequestColumns+" FROM chat_requests WHERE "+chatRequestKey, u.Name, ownerEpoch(u), session, id))
	if err == nil {
		if c.State == ChatAbandoned || c.State == ChatDeleted {
			return c, false, ErrChatGone
		}
		if c.Fingerprint != fingerprint {
			return c, false, ErrChatConflict
		}
		return c, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ChatRequest{}, false, err
	}
	if err = checkChatSession(tx, u, session); err != nil {
		return ChatRequest{}, false, err
	}
	if state == ChatAccepted {
		var n int
		if err = tx.QueryRow("SELECT count(*) FROM chat_requests WHERE session_id=? AND ("+chatRequestActive+" OR state='queued')", session).Scan(&n); err != nil {
			return ChatRequest{}, false, err
		}
		if n != 0 {
			return ChatRequest{}, false, ErrChatPending // queued messages go first
		}
	}
	now := usageTimestamp(time.Now())
	turn := NewID()
	if into != "" {
		turn = "" // runs inside the turn it was merged into
	}
	c = ChatRequest{RequestID: id, SessionID: session, ThreadID: in.ThreadID, TurnID: turn, State: state, Revision: 1, CreatedAt: now, UpdatedAt: now, MergedInto: into, Fingerprint: fingerprint}
	// Decode our serialized snapshot so the receipt cannot alias a caller's slice.
	if err = json.Unmarshal(raw, &c.Request); err != nil {
		return ChatRequest{}, false, err
	}
	_, err = tx.Exec(`INSERT INTO chat_requests(user,user_created_at,session_id,request_id,thread_id,turn_id,fingerprint,request_json,state,merged_into,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, u.Name, ownerEpoch(u), session, id, c.ThreadID, c.TurnID, fingerprint, string(raw), c.State, into, now, now)
	if err != nil {
		return ChatRequest{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return ChatRequest{}, false, err
	}
	return c, true, nil
}

// NextQueuedChatRequest is the session's oldest queued request and its owner,
// an internal dispatch read like PendingChatRequest; sql.ErrNoRows when none.
func (s *Store) NextQueuedChatRequest(session string) (ChatRequest, User, error) {
	var name, epoch string
	row := s.db.QueryRow("SELECT user,user_created_at,"+chatRequestColumns+" FROM chat_requests WHERE session_id=? AND state='queued' ORDER BY created_at, rowid LIMIT 1", session)
	var c ChatRequest
	var raw string
	err := row.Scan(&name, &epoch, &c.RequestID, &c.SessionID, &c.ThreadID, &c.TurnID, &raw, &c.State, &c.ErrorCode, &c.Revision, &c.CreatedAt, &c.UpdatedAt, &c.MergedInto, &c.Fingerprint)
	if err != nil {
		return c, User{}, err
	}
	if err = json.Unmarshal([]byte(raw), &c.Request); err != nil {
		return c, User{}, err
	}
	u, ok := s.GetUser(name)
	if !ok || ownerEpoch(u) != epoch {
		return c, User{}, sql.ErrNoRows // a replaced user's leftovers never run
	}
	return c, u, nil
}

// QueuedChatSessions lists sessions with queued requests, for dispatch after
// a restart.
func (s *Store) QueuedChatSessions() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT session_id FROM chat_requests WHERE state='queued'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// QueuedChatRequests lists the session's queue in order, so every page can
// show and withdraw it.
func (s *Store) QueuedChatRequests(u User, session string) ([]ChatRequest, error) {
	rows, err := s.db.Query("SELECT "+chatRequestColumns+" FROM chat_requests WHERE user=? AND user_created_at=? AND session_id=? AND state='queued' ORDER BY created_at, rowid LIMIT 50", u.Name, ownerEpoch(u), session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ChatRequest{}
	for rows.Next() {
		c, err := scanChatRequest(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func checkChatSession(tx *sql.Tx, u User, session string) error {
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM sessions WHERE id=? AND user=?", session, u.Name).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// ChatRequest also works after workspace deletion so the owner can recover the
// tombstone. A deleted/recreated user cannot read the old identity's records.
func (s *Store) ChatRequest(u User, session, id string) (ChatRequest, error) {
	return scanChatRequest(s.db.QueryRow("SELECT "+chatRequestColumns+" FROM chat_requests WHERE "+chatRequestKey+" AND EXISTS(SELECT 1 FROM users WHERE name=? AND created_at=?)", u.Name, ownerEpoch(u), session, id, u.Name, ownerEpoch(u)))
}

// ReplayChatRequest checks an existing envelope without admission checks or
// allocating work. Reading an accepted result remains possible after revocation.
func (s *Store) ReplayChatRequest(u User, session, id string, in ChatRequestInput) (ChatRequest, error) {
	_, fingerprint, err := encodeChatRequest(in)
	if err != nil {
		return ChatRequest{}, err
	}
	c, err := s.ChatRequest(u, session, id)
	if err != nil {
		return c, err
	}
	if c.State == ChatAbandoned || c.State == ChatDeleted {
		return c, ErrChatGone
	}
	if c.Fingerprint != fingerprint {
		return c, ErrChatConflict
	}
	return c, nil
}

// RecentChatRequests is bounded; callers also query PendingChatRequest so an
// unresolved request cannot disappear behind newer terminal records.
func (s *Store) RecentChatRequests(u User, session, thread string) ([]ChatRequest, error) {
	rows, err := s.db.Query("SELECT "+chatRequestColumns+" FROM chat_requests WHERE user=? AND user_created_at=? AND session_id=? AND (?='' OR thread_id=?) AND EXISTS(SELECT 1 FROM users WHERE name=? AND created_at=?) ORDER BY rowid DESC LIMIT 50", u.Name, ownerEpoch(u), session, thread, thread, u.Name, ownerEpoch(u))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ChatRequest{}
	for rows.Next() {
		c, err := scanChatRequest(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// VisitChatAttachmentInputs streams settled receipts until explicit deletion,
// without collecting all prompts in memory. The callback must not call Store
// while the single database connection is reading. Errors stop TTL cleanup.
func (s *Store) VisitChatAttachmentInputs(user string, visit func(ChatRequestInput)) error {
	rows, err := s.db.Query("SELECT request_json FROM chat_requests WHERE user=? AND request_json!='null'", user)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var in ChatRequestInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return err
		}
		visit(in)
	}
	return rows.Err()
}

// DeleteChatThreadRequests retains ID fences before the transcript is removed.
// The server also holds the room, preventing legacy or receipt turns from racing.
func (s *Store) DeleteChatThreadRequests(u User, session, thread string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return err
	}
	if err = checkChatSession(tx, u, session); err != nil {
		return err
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM chat_requests WHERE session_id=? AND "+chatRequestActive, session).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return ErrChatPending
	}
	_, err = tx.Exec("UPDATE chat_requests SET state='deleted',request_json='null',revision=revision+1,updated_at=? WHERE user=? AND user_created_at=? AND session_id=? AND thread_id=? AND state!='deleted'", usageTimestamp(time.Now()), u.Name, ownerEpoch(u), session, thread)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// PendingChatRequest is an internal room admission gate, not an authorized API
// read. Callers must fail closed on errors other than sql.ErrNoRows.
func (s *Store) PendingChatRequest(session string) (ChatRequest, error) {
	return scanChatRequest(s.db.QueryRow("SELECT "+chatRequestColumns+" FROM chat_requests WHERE session_id=? AND "+chatRequestActive, session))
}

func chatTransitionAllowed(from, to string) bool {
	switch from {
	case ChatQueued:
		// Taken from the queue, withdrawn by the user, or refused at dispatch
		// (thread changed, quota, account) without ever running.
		return to == ChatAccepted || to == ChatAbandoned || to == ChatFailed
	case ChatAccepted:
		return to == ChatStarting || to == ChatFailed || to == ChatUncertain
	case ChatStarting:
		return to == ChatRunning || to == ChatFailed || to == ChatUncertain
	case ChatRunning:
		// failed after running = the CLI exited with an error; it may have
		// changed files, but nothing from the turn is still executing.
		return to == ChatCompleted || to == ChatFailed || to == ChatInterrupted || to == ChatUncertain
	case ChatUncertain:
		return to == ChatReviewed
	}
	return false
}

// AdvanceChatRequest uses the observed revision, preventing two workers from
// acquiring the same stage and preventing stale completion after recovery.
// Persist starting/running BEFORE external execution; finalize only AFTER the
// existing usage flush. A reviewed uncertain turn is not a successful turn.
func (s *Store) AdvanceChatRequest(u User, session, id string, revision int64, state, code string) (ChatRequest, error) {
	if len(code) > 64 || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		return ChatRequest{}, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ChatRequest{}, err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return ChatRequest{}, err
	}
	c, err := scanChatRequest(tx.QueryRow("SELECT "+chatRequestColumns+" FROM chat_requests WHERE "+chatRequestKey, u.Name, ownerEpoch(u), session, id))
	if err != nil {
		return c, err
	}
	if c.Revision != revision || !chatTransitionAllowed(c.State, state) {
		return c, ErrChatConflict
	}
	if state == ChatReviewed {
		code = c.ErrorCode // Preserve the reason for uncertainty after review.
	}
	c.State, c.ErrorCode, c.UpdatedAt = state, code, usageTimestamp(time.Now())
	c.Revision++
	_, err = tx.Exec("UPDATE chat_requests SET state=?,error_code=?,revision=?,updated_at=? WHERE "+chatRequestKey, c.State, c.ErrorCode, c.Revision, c.UpdatedAt, u.Name, ownerEpoch(u), session, id)
	if err != nil {
		return ChatRequest{}, err
	}
	if err = tx.Commit(); err != nil {
		return ChatRequest{}, err
	}
	return c, nil
}

// AbandonUnknownChatRequest fences a delayed submission. It never cancels an
// accepted turn: callers must query/review that receipt instead. Repeating an
// already committed abandonment succeeds, including after workspace deletion.
func (s *Store) AbandonUnknownChatRequest(u User, session, id string) error {
	if !validChatKey(session) || !validChatKey(id) {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return err
	}
	c, err := scanChatRequest(tx.QueryRow("SELECT "+chatRequestColumns+" FROM chat_requests WHERE "+chatRequestKey, u.Name, ownerEpoch(u), session, id))
	if err == nil {
		if c.State == ChatAbandoned || c.State == ChatDeleted {
			return nil
		}
		return ErrChatConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = checkChatSession(tx, u, session); err != nil {
		return err
	}
	now := usageTimestamp(time.Now())
	_, err = tx.Exec(`INSERT INTO chat_requests(user,user_created_at,session_id,request_id,thread_id,turn_id,fingerprint,request_json,state,created_at,updated_at) VALUES(?,?,?,?,'','','','null','abandoned',?,?)`, u.Name, ownerEpoch(u), session, id, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverChatRequests runs once on server startup, before any room can accept
// work. A crash can happen on either side of an external side effect; no stage
// is automatically retried. Terminal states and tombstones are left intact.
func (s *Store) RecoverChatRequests() error {
	_, err := s.db.Exec("UPDATE chat_requests SET state='uncertain',error_code='server_restarted',revision=revision+1,updated_at=? WHERE state IN ('accepted','starting','running')", usageTimestamp(time.Now()))
	return err
}

// TurnReasoning is the reasoning effort a web chat turn was submitted with.
type TurnReasoning struct {
	Effort  string
	Control string
}

// TurnReasonings reads, from the chat receipts, the effort each listed turn
// was sent with. Turns without a receipt (terminal, title, legacy WebSocket),
// receipts whose body was cleared with a deleted thread or workspace, and
// turns sent without an explicit effort are absent from the result.
func (s *Store) TurnReasonings(turnIDs []string) (map[string]TurnReasoning, error) {
	out := map[string]TurnReasoning{}
	seen := map[string]bool{}
	ids := make([]any, 0, len(turnIDs))
	for _, id := range turnIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 200)]
		ids = ids[len(chunk):]
		rows, err := s.db.Query("SELECT turn_id, request_json FROM chat_requests WHERE turn_id != '' AND turn_id IN (?"+strings.Repeat(",?", len(chunk)-1)+")", chunk...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var turn, raw string
			if err := rows.Scan(&turn, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var in ChatRequestInput
			if json.Unmarshal([]byte(raw), &in) == nil && in.Effort != "" {
				out[turn] = TurnReasoning{Effort: in.Effort, Control: in.EffortControl}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
