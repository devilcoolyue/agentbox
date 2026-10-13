package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Delegation states. routing/running are live only while the main agent turn
// that issued the delegate call is still running; a restart turns them into
// uncertain, which is never re-run.
const (
	DelegationRouting   = "routing"
	DelegationRunning   = "running"
	DelegationCompleted = "completed"
	DelegationFailed    = "failed"
	DelegationDeclined  = "declined"
	DelegationCancelled = "cancelled"
	DelegationUncertain = "uncertain"
)

var ErrDelegationNotFound = errors.New("delegation not found")

type Delegation struct {
	ID              string          `json:"id"`
	User            string          `json:"-"`
	SessionID       string          `json:"session_id"`
	ThreadID        string          `json:"thread_id"`
	TurnID          string          `json:"turn_id"`
	Seq             int             `json:"seq"`
	State           string          `json:"state"`
	TaskType        string          `json:"task_type"`
	Risks           []string        `json:"risks"`
	Access          string          `json:"access"`
	Task            string          `json:"task"`
	Route           json.RawMessage `json:"route,omitempty"`
	WorkerSessionID string          `json:"-"`
	Agent           string          `json:"agent,omitempty"`
	AccountID       string          `json:"account_id,omitempty"`
	Model           string          `json:"model,omitempty"`
	ProviderSession string          `json:"-"`
	ContinueOf      string          `json:"continue_of,omitempty"`
	Report          string          `json:"report,omitempty"`
	Changes         json.RawMessage `json:"changes,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
	Feedback        string          `json:"feedback,omitempty"`
	FeedbackNote    string          `json:"feedback_note,omitempty"`
	CreatedAt       string          `json:"created_at"`
	StartedAt       string          `json:"started_at,omitempty"`
	FinishedAt      string          `json:"finished_at,omitempty"`
}

const delegationCols = "id,user,session_id,thread_id,turn_id,seq,state,task_type,risks_json,access,task,route_json,worker_session_id,agent,account_id,model,provider_session,continue_of,report,changes_json,error_code,feedback,feedback_note,created_at,started_at,finished_at"

func scanDelegation(row interface{ Scan(...any) error }) (Delegation, error) {
	var d Delegation
	var risks, route, changes string
	err := row.Scan(&d.ID, &d.User, &d.SessionID, &d.ThreadID, &d.TurnID, &d.Seq, &d.State, &d.TaskType, &risks, &d.Access, &d.Task, &route,
		&d.WorkerSessionID, &d.Agent, &d.AccountID, &d.Model, &d.ProviderSession, &d.ContinueOf, &d.Report, &changes, &d.ErrorCode,
		&d.Feedback, &d.FeedbackNote, &d.CreatedAt, &d.StartedAt, &d.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrDelegationNotFound
	}
	if err != nil {
		return d, err
	}
	_ = json.Unmarshal([]byte(risks), &d.Risks)
	if d.Risks == nil {
		d.Risks = []string{}
	}
	if route != "null" && route != "" {
		d.Route = json.RawMessage(route)
	}
	if changes != "null" && changes != "" {
		d.Changes = json.RawMessage(changes)
	}
	return d, nil
}

// CreateDelegation records a delegate call before any routing decision or
// provider request, numbering it within the main turn.
func (s *Store) CreateDelegation(d Delegation) (Delegation, error) {
	if !validChatKey(d.ID) || d.User == "" || d.SessionID == "" || d.TurnID == "" || (d.Access != "read" && d.Access != "write") {
		return d, ErrChatInvalid
	}
	risks, err := json.Marshal(d.Risks)
	if err != nil {
		return d, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if err = tx.QueryRow("SELECT COALESCE(MAX(seq),0)+1 FROM delegations WHERE turn_id=?", d.TurnID).Scan(&d.Seq); err != nil {
		return d, err
	}
	d.State = DelegationRouting
	d.CreatedAt = usageTimestamp(time.Now())
	if _, err = tx.Exec("INSERT INTO delegations(id,user,session_id,thread_id,turn_id,seq,state,task_type,risks_json,access,task,continue_of,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
		d.ID, d.User, d.SessionID, d.ThreadID, d.TurnID, d.Seq, d.State, d.TaskType, string(risks), d.Access, d.Task, d.ContinueOf, d.CreatedAt); err != nil {
		return d, err
	}
	return d, tx.Commit()
}

func rawOrNull(v json.RawMessage) string {
	if len(v) == 0 {
		return "null"
	}
	return string(v)
}

// RouteDelegation saves the routing outcome. A declined call ends here; a
// chosen worker moves to running with its binding fixed for the whole call.
func (s *Store) RouteDelegation(id string, route json.RawMessage, worker Delegation, declined bool) (Delegation, error) {
	state, started, finished := DelegationRunning, usageTimestamp(time.Now()), ""
	if declined {
		state, started, finished = DelegationDeclined, "", usageTimestamp(time.Now())
	}
	res, err := s.db.Exec("UPDATE delegations SET state=?,route_json=?,worker_session_id=?,agent=?,account_id=?,model=?,started_at=?,finished_at=? WHERE id=? AND state=?",
		state, rawOrNull(route), worker.WorkerSessionID, worker.Agent, worker.AccountID, worker.Model, started, finished, id, DelegationRouting)
	if err != nil {
		return Delegation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Delegation{}, ErrChatConflict
	}
	return s.delegation(id)
}

func (s *Store) SaveDelegationProviderSession(id, provider string) error {
	_, err := s.db.Exec("UPDATE delegations SET provider_session=? WHERE id=? AND state=?", provider, id, DelegationRunning)
	return err
}

// FinishDelegation closes a running call exactly once.
func (s *Store) FinishDelegation(id, state, report string, changes json.RawMessage, code string) (Delegation, error) {
	switch state {
	case DelegationCompleted, DelegationFailed, DelegationCancelled, DelegationUncertain:
	default:
		return Delegation{}, ErrChatInvalid
	}
	res, err := s.db.Exec("UPDATE delegations SET state=?,report=?,changes_json=?,error_code=?,finished_at=? WHERE id=? AND state IN (?,?)",
		state, report, rawOrNull(changes), code, usageTimestamp(time.Now()), id, DelegationRouting, DelegationRunning)
	if err != nil {
		return Delegation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Delegation{}, ErrChatConflict
	}
	return s.delegation(id)
}

func (s *Store) delegation(id string) (Delegation, error) {
	return scanDelegation(s.db.QueryRow("SELECT "+delegationCols+" FROM delegations WHERE id=?", id))
}

// Delegation returns a call owned by the user; other users' IDs look absent.
func (s *Store) Delegation(user, id string) (Delegation, error) {
	return scanDelegation(s.db.QueryRow("SELECT "+delegationCols+" FROM delegations WHERE id=? AND user=?", id, user))
}

// TurnDelegation finds an earlier call of the same workspace that a follow-up
// "continue" may resume.
func (s *Store) SessionDelegation(session, id string) (Delegation, error) {
	return scanDelegation(s.db.QueryRow("SELECT "+delegationCols+" FROM delegations WHERE id=? AND session_id=?", id, session))
}

// SessionDelegations lists a workspace's calls, oldest first, at most limit.
func (s *Store) SessionDelegations(session string, limit int) ([]Delegation, error) {
	rows, err := s.db.Query("SELECT "+delegationCols+" FROM delegations WHERE session_id=? ORDER BY created_at, seq LIMIT ?", session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delegation{}
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RateDelegation stores the main agent's review verdict for a finished call.
func (s *Store) RateDelegation(session, turn, id, verdict, note string) error {
	switch verdict {
	case "ok", "minor_fix", "redo", "abandoned":
	default:
		return ErrChatInvalid
	}
	res, err := s.db.Exec("UPDATE delegations SET feedback=?,feedback_note=? WHERE id=? AND session_id=? AND turn_id=? AND state IN (?,?)",
		verdict, note, id, session, turn, DelegationCompleted, DelegationFailed)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrDelegationNotFound
	}
	return nil
}

// DelegationCost sums every recorded worker row, including Jev routing calls.
func (s *Store) DelegationCost(id string) int64 {
	var cost int64
	_ = s.db.QueryRow("SELECT COALESCE(SUM(cost_micro_usd),0) FROM usage_events WHERE delegation_id=?", id).Scan(&cost)
	return cost
}

// RecoverDelegations runs before the server accepts requests: no delegate call
// can still be running, and none is ever restarted automatically.
func (s *Store) RecoverDelegations() error {
	_, err := s.db.Exec("UPDATE delegations SET state=?,error_code='server_restarted',finished_at=? WHERE state IN (?,?)",
		DelegationUncertain, usageTimestamp(time.Now()), DelegationRouting, DelegationRunning)
	return err
}

type DelegationStat struct {
	AccountID    string `json:"account_id"`
	Model        string `json:"model"`
	TaskType     string `json:"task_type"`
	Total        int    `json:"total"`
	OK           int    `json:"ok"`
	MinorFix     int    `json:"minor_fix"`
	Redo         int    `json:"redo"`
	Abandoned    int    `json:"abandoned"`
	Failed       int    `json:"failed"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
}

// DelegationStats aggregates every call that reached a worker, grouped by the
// worker model and task type, as evidence for adjusting the model tags.
func (s *Store) DelegationStats() ([]DelegationStat, error) {
	rows, err := s.db.Query(`SELECT d.account_id, d.model, d.task_type, COUNT(*),
		SUM(d.feedback='ok'), SUM(d.feedback='minor_fix'), SUM(d.feedback='redo'), SUM(d.feedback='abandoned'),
		SUM(d.state IN ('failed','cancelled','uncertain')),
		COALESCE(SUM((SELECT SUM(u.cost_micro_usd) FROM usage_events u WHERE u.delegation_id=d.id AND u.kind='chat')),0)
		FROM delegations d WHERE d.account_id != '' AND d.state NOT IN ('routing','declined')
		GROUP BY d.account_id, d.model, d.task_type ORDER BY d.account_id, d.model, d.task_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DelegationStat{}
	for rows.Next() {
		var st DelegationStat
		if err := rows.Scan(&st.AccountID, &st.Model, &st.TaskType, &st.Total, &st.OK, &st.MinorFix, &st.Redo, &st.Abandoned, &st.Failed, &st.CostMicroUSD); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// --- worker workspaces ---

// PublicSessions lists the user's own workspaces, never their hidden workers.
func (s *Store) PublicSessions(user string) ([]Session, error) {
	rows, err := s.db.Query("SELECT "+sessionCols+" FROM sessions WHERE user=? AND NOT EXISTS(SELECT 1 FROM delegation_workers WHERE session_id=sessions.id) ORDER BY created_at DESC", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, e := scanSession(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DelegationOwner reports the workspace a hidden worker session serves.
func (s *Store) DelegationOwner(id string) (string, bool, error) {
	var owner string
	err := s.db.QueryRow("SELECT owner_session_id FROM delegation_workers WHERE session_id=?", id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return owner, err == nil, err
}

// DelegationWorkerIDs returns the IDs of every hidden worker session.
func (s *Store) DelegationWorkerIDs() map[string]bool {
	out := map[string]bool{}
	rows, err := s.db.Query("SELECT session_id FROM delegation_workers")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// WorkerSession returns the hidden worker of owner for one account.
func (s *Store) WorkerSession(owner, account string) (Session, bool) {
	var id string
	if s.db.QueryRow("SELECT session_id FROM delegation_workers WHERE owner_session_id=? AND account_id=?", owner, account).Scan(&id) != nil {
		return Session{}, false
	}
	return s.Get(id)
}

// WorkerSessions lists the hidden workers of one owner workspace.
func (s *Store) WorkerSessions(owner string) []Session {
	return s.query(" WHERE id IN (SELECT session_id FROM delegation_workers WHERE owner_session_id = ?)", owner)
}

// CreateWorkerSession inserts the hidden session and its marker atomically so a
// half-created worker can never appear as an ordinary workspace.
func (s *Store) CreateWorkerSession(owner string, worker Session) (Session, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM sessions WHERE id=? AND user=?", owner, worker.User).Scan(&n); err != nil {
		return Session{}, err
	}
	if n != 1 {
		return Session{}, sql.ErrNoRows
	}
	var existing string
	err = tx.QueryRow("SELECT session_id FROM delegation_workers WHERE owner_session_id=? AND account_id=?", owner, worker.AccountID).Scan(&existing)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return Session{}, err
		}
		if sess, ok := s.Get(existing); ok {
			return sess, nil
		}
		return Session{}, sql.ErrNoRows
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Session{}, err
	}
	now := time.Now()
	worker.CreatedAt, worker.UpdatedAt = now, now
	if err = s.put(tx, worker); err != nil {
		return Session{}, err
	}
	if _, err = tx.Exec("INSERT INTO delegation_workers(session_id,owner_session_id,account_id,created_at) VALUES(?,?,?,?)", worker.ID, owner, worker.AccountID, usageTimestamp(now)); err != nil {
		return Session{}, err
	}
	return worker, tx.Commit()
}
