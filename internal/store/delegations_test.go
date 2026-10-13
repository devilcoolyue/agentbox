package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Upgrading from the collaboration preview removes its hidden participant
// workspaces and tables, keeps every usage row, and leaves chat working.
func TestDelegationMigrationRetiresCollaboration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db, migrations()[:17]); err != nil {
		t.Fatal(err)
	}
	now := "2026-10-12T00:00:00.000000000Z"
	for _, q := range []string{
		`INSERT INTO sessions(id,user,name,agent,account_id,container_id,status,chat_session,stop_reason,default_model,created_at,updated_at) VALUES('space','alice','space','claude','a','','stopped','','','m','` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user,name,agent,account_id,container_id,status,chat_session,stop_reason,default_model,created_at,updated_at) VALUES('participant','alice','p','codex','b','','stopped','','','m','` + now + `','` + now + `')`,
		`INSERT INTO collaborations(id,user,user_created_at,source_session_id,request_id,request_json,fingerprint,state,created_at,updated_at) VALUES('run','alice','','space','req','{}','f','completed','` + now + `','` + now + `')`,
		`INSERT INTO collaboration_participants(run_id,role,generation,config_version,session_id,created_at) VALUES('run','executor',1,1,'participant','` + now + `')`,
		`INSERT INTO usage_events(ts,user,session_id,turn_id,agent,kind,cost_micro_usd) VALUES('` + now + `','alice','participant','t','codex','chat',7)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.Get("participant"); ok {
		t.Fatal("hidden participant survived as a workspace")
	}
	if _, ok := s.Get("space"); !ok {
		t.Fatal("ordinary workspace removed")
	}
	var tables int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type IN ('table','trigger') AND (name LIKE '%collaboration%' OR name='decision_attempts')").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("collaboration schema left behind", tables, err)
	}
	rows := s.ListUsage(UsageFilter{})
	if len(rows) != 1 || rows[0].CostMicroUSD != 7 || rows[0].DelegationID != "" {
		t.Fatal("usage history changed", rows)
	}
	// The trigger once attached to chat_requests must be gone with its table.
	if _, err := s.db.Exec("INSERT INTO chat_requests(user,user_created_at,session_id,request_id,thread_id,turn_id,fingerprint,request_json,state,created_at,updated_at) VALUES('alice','','space','r','t','u','f','{}','completed',?,?)", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE chat_requests SET request_json='null' WHERE request_id='r'"); err != nil {
		t.Fatal("stale collaboration trigger fired", err)
	}
}

func delegationFixture(t *testing.T) (*Store, Session) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	owner := Session{ID: "owner", User: "alice", Name: "space", Agent: "claude", AccountID: "a", Status: StatusStopped, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.Put(owner); err != nil {
		t.Fatal(err)
	}
	return s, owner
}

func TestDelegationLifecycleAndStats(t *testing.T) {
	s, owner := delegationFixture(t)
	if _, err := s.CreateDelegation(Delegation{ID: "bad id", User: "alice", SessionID: owner.ID, TurnID: "t", Access: "write"}); !errors.Is(err, ErrChatInvalid) {
		t.Fatal(err)
	}
	d, err := s.CreateDelegation(Delegation{ID: strings.Repeat("a", 32), User: "alice", SessionID: owner.ID, ThreadID: "th", TurnID: "turn", TaskType: "docs", Risks: []string{"auth"}, Access: "write", Task: "write docs"})
	if err != nil || d.Seq != 1 || d.State != DelegationRouting {
		t.Fatal(d, err)
	}
	second, err := s.CreateDelegation(Delegation{ID: strings.Repeat("b", 32), User: "alice", SessionID: owner.ID, ThreadID: "th", TurnID: "turn", TaskType: "docs", Access: "read", Task: "look"})
	if err != nil || second.Seq != 2 {
		t.Fatal(second, err)
	}
	if _, err = s.RouteDelegation(second.ID, json.RawMessage(`{"source":"rules","reason":"no_eligible_worker"}`), Delegation{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RouteDelegation(second.ID, nil, Delegation{}, true); !errors.Is(err, ErrChatConflict) {
		t.Fatal("routed twice", err)
	}
	d, err = s.RouteDelegation(d.ID, json.RawMessage(`{"source":"rules"}`), Delegation{Agent: "claude", AccountID: "a", Model: "sonnet"}, false)
	if err != nil || d.State != DelegationRunning || d.StartedAt == "" {
		t.Fatal(d, err)
	}
	if err = s.SaveDelegationProviderSession(d.ID, "provider-1"); err != nil {
		t.Fatal(err)
	}
	if err = s.RateDelegation(owner.ID, "turn", d.ID, "ok", ""); !errors.Is(err, ErrDelegationNotFound) {
		t.Fatal("rated an unfinished call", err)
	}
	if err = s.InsertUsage(UsageEvent{User: "alice", SessionID: owner.ID, TurnID: "w", Agent: "claude", Model: "sonnet", Kind: UsageKindChat, CostMicroUSD: 120, DelegationID: d.ID}); err != nil {
		t.Fatal(err)
	}
	d, err = s.FinishDelegation(d.ID, DelegationCompleted, "## Done", json.RawMessage(`{"known":true}`), "")
	if err != nil || d.State != DelegationCompleted || d.ProviderSession != "provider-1" || string(d.Changes) != `{"known":true}` {
		t.Fatal(d, err)
	}
	if _, err = s.FinishDelegation(d.ID, DelegationFailed, "", nil, "x"); !errors.Is(err, ErrChatConflict) {
		t.Fatal("finished twice", err)
	}
	if err = s.RateDelegation(owner.ID, "other-turn", d.ID, "ok", ""); !errors.Is(err, ErrDelegationNotFound) {
		t.Fatal("rated from another turn", err)
	}
	if err = s.RateDelegation(owner.ID, "turn", d.ID, "minor_fix", "small"); err != nil {
		t.Fatal(err)
	}
	if got := s.DelegationCost(d.ID); got != 120 {
		t.Fatal(got)
	}
	stats, err := s.DelegationStats()
	if err != nil || len(stats) != 1 || stats[0].Model != "sonnet" || stats[0].Total != 1 || stats[0].MinorFix != 1 || stats[0].CostMicroUSD != 120 {
		t.Fatalf("%+v %v", stats, err)
	}
	if only := s.ListUsage(UsageFilter{Delegation: "only"}); len(only) != 1 {
		t.Fatal(only)
	}
	if none := s.ListUsage(UsageFilter{Delegation: "exclude"}); len(none) != 0 {
		t.Fatal(none)
	}
	if _, err = s.Delegation("bob", d.ID); !errors.Is(err, ErrDelegationNotFound) {
		t.Fatal("another user read a delegation", err)
	}
	if err = s.RecoverDelegations(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Delegation("alice", d.ID); got.State != DelegationCompleted {
		t.Fatal("recovery changed a finished call", got.State)
	}
}

func TestRecoverDelegationsNeverResumesRunningCalls(t *testing.T) {
	s, owner := delegationFixture(t)
	d, err := s.CreateDelegation(Delegation{ID: strings.Repeat("c", 32), User: "alice", SessionID: owner.ID, TurnID: "turn", TaskType: "docs", Access: "write", Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverDelegations(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Delegation("alice", d.ID); got.State != DelegationUncertain || got.ErrorCode != "server_restarted" {
		t.Fatal(got)
	}
}

func TestWorkerSessionsStayHiddenAndLeaveWithOwner(t *testing.T) {
	s, owner := delegationFixture(t)
	worker, err := s.CreateWorkerSession(owner.ID, Session{ID: "worker", User: "alice", Name: "w", Agent: "codex", AccountID: "b", Status: StatusStopped})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateWorkerSession(owner.ID, Session{ID: "other", User: "alice", Agent: "codex", AccountID: "b"})
	if err != nil || again.ID != worker.ID {
		t.Fatal("second worker for the same account", again, err)
	}
	if _, err = s.CreateWorkerSession(owner.ID, Session{ID: "foreign", User: "bob", AccountID: "c"}); err == nil {
		t.Fatal("worker created for another user's workspace")
	}
	public, err := s.PublicSessions("alice")
	if err != nil || len(public) != 1 || public[0].ID != owner.ID {
		t.Fatal("worker listed as a workspace", public, err)
	}
	if id, ok, err := s.DelegationOwner("worker"); err != nil || !ok || id != owner.ID {
		t.Fatal(id, ok, err)
	}
	if got, ok := s.WorkerSession(owner.ID, "b"); !ok || got.ID != "worker" || len(s.WorkerSessions(owner.ID)) != 1 {
		t.Fatal(got, ok)
	}
	d, err := s.CreateDelegation(Delegation{ID: strings.Repeat("d", 32), User: "alice", SessionID: owner.ID, TurnID: "turn", TaskType: "docs", Access: "write", Task: "private text"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("worker"); ok {
		t.Fatal("worker survived its owner")
	}
	if _, ok, _ := s.DelegationOwner("worker"); ok {
		t.Fatal("worker marker survived")
	}
	if got, err := s.Delegation("alice", d.ID); err != nil || got.Task != "" || got.TaskType != "docs" {
		t.Fatal("deleted workspace kept task text or lost metadata", got, err)
	}
	if err = s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delegation("alice", d.ID); !errors.Is(err, ErrDelegationNotFound) {
		t.Fatal("user deletion kept delegations", err)
	}
}

func TestChatRequestDelegationIsValidatedAndFingerprinted(t *testing.T) {
	base := ChatRequestInput{Scope: strings.Repeat("a", 64), ThreadID: "thread", Text: "hi"}
	_, plain, err := encodeChatRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	with := base
	with.Delegation = &DelegationOptions{Version: 1, Workers: []DelegationWorker{{AccountID: "a", Model: "sonnet"}}, Jev: true}
	_, delegated, err := encodeChatRequest(with)
	if err != nil || delegated == plain {
		t.Fatal("delegation must change the fingerprint", err)
	}
	for _, bad := range []*DelegationOptions{
		{Version: 2, Workers: []DelegationWorker{{AccountID: "a", Model: "m"}}},
		{Version: 1},
		{Version: 1, Workers: []DelegationWorker{{AccountID: "a", Model: "m"}, {AccountID: "a", Model: "m"}}},
		{Version: 1, Workers: []DelegationWorker{{AccountID: "", Model: "m"}}},
	} {
		in := base
		in.Delegation = bad
		if _, _, err := encodeChatRequest(in); !errors.Is(err, ErrChatInvalid) {
			t.Fatal("accepted", bad)
		}
	}
}
