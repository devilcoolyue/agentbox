package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func chatRequestFixture(t *testing.T) (*Store, User, Session, ChatRequestInput) {
	t.Helper()
	s, u, c := creationFixture(t)
	if err := s.Put(c.Session); err != nil {
		t.Fatal(err)
	}
	return s, u, c.Session, ChatRequestInput{Scope: "instance-user-scope", ThreadID: "thread", Text: "synthetic prompt", Model: "saved-model", Effort: "high", EffortControl: "native", Attachments: []string{"/shared/.file/fixture.txt"}}
}

func advanceChat(t *testing.T, s *Store, u User, c ChatRequest, states ...string) ChatRequest {
	t.Helper()
	for _, state := range states {
		var err error
		c, err = s.AdvanceChatRequest(u, c.SessionID, c.RequestID, c.Revision, state, "")
		if err != nil {
			t.Fatalf("advance to %s: %v", state, err)
		}
	}
	return c
}

func TestChatRequestConcurrentAcceptanceAndFrozenEnvelope(t *testing.T) {
	s, u, sess, input := chatRequestFixture(t)
	var wg sync.WaitGroup
	var freshCount atomic.Int32
	results := make(chan ChatRequest, 24)
	for range 24 {
		wg.Go(func() {
			c, fresh, err := s.AcceptChatRequest(u, sess.ID, "request", input)
			if err != nil {
				t.Error(err)
				return
			}
			if fresh {
				freshCount.Add(1)
			}
			results <- c
		})
	}
	wg.Wait()
	close(results)
	if freshCount.Load() != 1 {
		t.Fatal("expected exactly one permission to schedule", freshCount.Load())
	}
	var first ChatRequest
	for c := range results {
		if first.TurnID == "" {
			first = c
		}
		if !reflect.DeepEqual(first, c) || c.State != ChatAccepted || c.Revision != 1 || !reflect.DeepEqual(*c.Request, input) {
			t.Fatal("replay lost frozen envelope/identity", c)
		}
	}
	for name, change := range map[string]func(*ChatRequestInput){
		"scope":       func(i *ChatRequestInput) { i.Scope = "other-scope" },
		"thread":      func(i *ChatRequestInput) { i.ThreadID = "other-thread" },
		"text":        func(i *ChatRequestInput) { i.Text += " changed" },
		"model":       func(i *ChatRequestInput) { i.Model = "other-model" },
		"effort":      func(i *ChatRequestInput) { i.Effort = "low" },
		"control":     func(i *ChatRequestInput) { i.EffortControl = "budget" },
		"attachments": func(i *ChatRequestInput) { i.Attachments = []string{"/shared/.file/other.txt"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := input
			change(&changed)
			if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "request", changed); fresh || !errors.Is(err, ErrChatConflict) {
				t.Fatal("ID accepted changed envelope", fresh, err)
			}
		})
	}
	input.Attachments[0] = "caller changed slice"
	if first.Request.Attachments[0] != "/shared/.file/fixture.txt" {
		t.Fatal("receipt aliases caller memory")
	}
	if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "second", input); fresh || !errors.Is(err, ErrChatPending) {
		t.Fatal("second ID admitted while first pending", fresh, err)
	}
	// Independent sessions do not block one another.
	sess.ID = "other-space"
	if err := s.Put(sess); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "request", input); err != nil || !fresh {
		t.Fatal("independent space blocked", err)
	}
}

func TestChatRequestStageCASAndUnknownReview(t *testing.T) {
	s, u, sess, input := chatRequestFixture(t)
	c, _, err := s.AcceptChatRequest(u, sess.ID, "request", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdvanceChatRequest(u, sess.ID, c.RequestID, c.Revision, ChatCompleted, ""); !errors.Is(err, ErrChatConflict) {
		t.Fatal("accepted request pretended complete", err)
	}
	var wg sync.WaitGroup
	var winners atomic.Int32
	for range 16 {
		wg.Go(func() {
			_, err := s.AdvanceChatRequest(u, sess.ID, c.RequestID, c.Revision, ChatStarting, "")
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrChatConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("two workers acquired starting", winners.Load())
	}
	c, err = s.ChatRequest(u, sess.ID, c.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	c = advanceChat(t, s, u, c, ChatRunning)
	if _, err = s.AdvanceChatRequest(u, sess.ID, c.RequestID, c.Revision, ChatFailed, ""); !errors.Is(err, ErrChatConflict) {
		t.Fatal("post-invocation error converted to safe failure", err)
	}
	if err = s.RecoverChatRequests(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdvanceChatRequest(u, sess.ID, c.RequestID, c.Revision, ChatCompleted, ""); !errors.Is(err, ErrChatConflict) {
		t.Fatal("stale worker overwrote restart recovery", err)
	}
	pending, err := s.PendingChatRequest(sess.ID)
	if err != nil || pending.State != ChatUncertain || pending.ErrorCode != "server_restarted" {
		t.Fatal("restart lost uncertainty", pending, err)
	}
	if err = s.AbandonUnknownChatRequest(u, sess.ID, c.RequestID); !errors.Is(err, ErrChatConflict) {
		t.Fatal("abandon hid accepted result", err)
	}
	if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "next", input); fresh || !errors.Is(err, ErrChatPending) {
		t.Fatal("uncertain result allowed new turn", err)
	}
	reviewed := advanceChat(t, s, u, pending, ChatReviewed)
	if reviewed.ErrorCode != "server_restarted" {
		t.Fatal("review erased uncertainty reason")
	}
	if _, err = s.PendingChatRequest(sess.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("review did not release admission gate", err)
	}
	if old, fresh, err := s.AcceptChatRequest(u, sess.ID, c.RequestID, input); err != nil || fresh || old.State != ChatReviewed {
		t.Fatal("review reran old ID", old, fresh, err)
	}
	if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "next", input); err != nil || !fresh {
		t.Fatal("new ID not available after review", fresh, err)
	}
}

func TestChatRequestAdmissionAcrossConnections(t *testing.T) {
	s, u, sess, input := chatRequestFixture(t)
	var seq int
	var name, path string
	if err := s.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	var winners atomic.Int32
	for i := range 20 {
		wg.Go(func() {
			connection := s
			if i%2 == 1 {
				connection = other
			}
			_, fresh, err := connection.AcceptChatRequest(u, sess.ID, "same-id", input)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				winners.Add(1)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("connections admitted same ID more than once", winners.Load())
	}
	c, err := s.ChatRequest(u, sess.ID, "same-id")
	if err != nil {
		t.Fatal(err)
	}
	advanceChat(t, s, u, c, ChatFailed)
	winners.Store(0)
	for i := range 20 {
		wg.Go(func() {
			connection := s
			if i%2 == 1 {
				connection = other
			}
			_, fresh, err := connection.AcceptChatRequest(u, sess.ID, NewID(), input)
			if err != nil && !errors.Is(err, ErrChatPending) {
				t.Error(err)
			}
			if fresh {
				winners.Add(1)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("connections admitted concurrent turns", winners.Load())
	}
}

func TestChatRequestAbandonRaceAndIdentityIsolation(t *testing.T) {
	s, u, sess, input := chatRequestFixture(t)
	if err := s.CreateUser(User{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	bob, _ := s.GetUser("bob")
	if _, _, err := s.AcceptChatRequest(bob, sess.ID, "request", input); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign workspace admitted", err)
	}
	if err := s.AbandonUnknownChatRequest(bob, sess.ID, "request"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign workspace fenced", err)
	}
	for range 12 {
		id := NewID()
		var wg sync.WaitGroup
		wg.Go(func() {
			_, _, err := s.AcceptChatRequest(u, sess.ID, id, input)
			if err != nil && !errors.Is(err, ErrChatGone) {
				t.Error(err)
			}
		})
		wg.Go(func() {
			err := s.AbandonUnknownChatRequest(u, sess.ID, id)
			if err != nil && !errors.Is(err, ErrChatConflict) {
				t.Error(err)
			}
		})
		wg.Wait()
		c, err := s.ChatRequest(u, sess.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if c.State == ChatAccepted {
			advanceChat(t, s, u, c, ChatFailed)
		} else if c.State != ChatAbandoned || c.Request != nil || c.TurnID != "" {
			t.Fatal("race returned unexpected receipt", c)
		}
		if c.State == ChatAbandoned {
			if err := s.AbandonUnknownChatRequest(u, sess.ID, id); err != nil {
				t.Fatal("lost abandonment acknowledgement not recoverable", err)
			}
		}
	}
	if err := s.AbandonUnknownChatRequest(u, sess.ID, "fenced"); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "fenced", input); fresh || !errors.Is(err, ErrChatGone) {
		t.Fatal("delayed request crossed abandonment fence", fresh, err)
	}
	if _, err := s.ChatRequest(bob, sess.ID, "fenced"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign receipt read", err)
	}
	if err := s.DeleteUser(u.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(User{Name: u.Name, CreatedAt: u.CreatedAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	replacement, _ := s.GetUser(u.Name)
	for _, actor := range []User{u, replacement} {
		if _, err := s.ChatRequest(actor, sess.ID, "fenced"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("old receipt visible after user recreation", err)
		}
	}
	if _, _, err := s.AcceptChatRequest(u, sess.ID, "late", input); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old identity still admitted", err)
	}
	if err := s.AbandonUnknownChatRequest(u, sess.ID, "late"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old identity still mutates receipts", err)
	}
	if _, fresh, err := s.AcceptChatRequest(replacement, sess.ID, "fenced", input); err != nil || !fresh {
		t.Fatal("new lifetime inherited old fence", fresh, err)
	}
}

func TestChatRequestWriteFailureAndDeleteAreAtomic(t *testing.T) {
	s, u, sess, input := chatRequestFixture(t)
	if _, err := s.db.Exec(`CREATE TRIGGER chat_insert_failure BEFORE INSERT ON chat_requests BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "request", input); err == nil || fresh {
		t.Fatal("failed persistence granted execution permission", fresh, err)
	}
	if _, err := s.PendingChatRequest(sess.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("failed transaction left receipt", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER chat_insert_failure"); err != nil {
		t.Fatal(err)
	}
	c, _, err := s.AcceptChatRequest(u, sess.ID, "request", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER chat_update_failure BEFORE UPDATE ON chat_requests BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdvanceChatRequest(u, sess.ID, c.RequestID, c.Revision, ChatStarting, ""); err == nil {
		t.Fatal("failed persistence advanced execution")
	}
	if err = s.Delete(sess.ID); err == nil {
		t.Fatal("session deleted without receipt tombstone")
	}
	if _, ok := s.Get(sess.ID); !ok {
		t.Fatal("deletion failed to roll back")
	}
	if err = s.DeleteUser(u.Name); err == nil {
		t.Fatal("user deleted without receipt redaction")
	}
	if _, ok := s.GetUser(u.Name); !ok {
		t.Fatal("user deletion failed to roll back")
	}
	if _, err = s.db.Exec("DROP TRIGGER chat_update_failure"); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.ChatRequest(u, sess.ID, c.RequestID)
	if err != nil || deleted.State != ChatDeleted || deleted.Request != nil || deleted.Fingerprint != c.Fingerprint || deleted.TurnID != c.TurnID {
		t.Fatal("deletion lost fence or retained prompt", deleted, err)
	}
	if _, fresh, err := s.AcceptChatRequest(u, sess.ID, c.RequestID, input); fresh || !errors.Is(err, ErrChatGone) {
		t.Fatal("deleted receipt replayed", fresh, err)
	}
}

func TestChatRequestInputAndFullDurability(t *testing.T) {
	s, u, sess, input := chatRequestFixture(t)
	for _, mutate := range []func(*ChatRequestInput){
		func(i *ChatRequestInput) { i.Text = " \n" },
		func(i *ChatRequestInput) { i.Text = strings.Repeat("a", (1<<20)+1) },
		func(i *ChatRequestInput) { i.Text = string([]byte{0xff}) },
		func(i *ChatRequestInput) { i.Attachments = make([]string, 65) },
		func(i *ChatRequestInput) { i.ThreadID = "../other" },
	} {
		bad := input
		mutate(&bad)
		if _, fresh, err := s.AcceptChatRequest(u, sess.ID, "request", bad); fresh || !errors.Is(err, ErrChatInvalid) {
			t.Fatal("invalid envelope admitted", err)
		}
	}
	for range 2 {
		var mode int
		if err := s.db.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil || mode != 2 {
			t.Fatal("receipt connection is not FULL", mode, err)
		}
		// Closing the idle connection forces the next read to use the DSN again.
		s.db.SetMaxIdleConns(0)
		s.db.SetMaxIdleConns(1)
	}
}

func TestChatRequestAbruptExitRecovery(t *testing.T) {
	const helperEnv = "AGENTBOX_CHAT_RECEIPT_CRASH_FIXTURE"
	if path := os.Getenv(helperEnv); path != "" {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
			t.Fatal(err)
		}
		if err = s.CreateUser(User{Name: "alice"}); err != nil {
			t.Fatal(err)
		}
		u, _ := s.GetUser("alice")
		for _, state := range []string{ChatAccepted, ChatStarting, ChatRunning, ChatCompleted, ChatFailed, ChatInterrupted, ChatUncertain, ChatReviewed} {
			if err = s.Put(Session{ID: state, User: u.Name}); err != nil {
				t.Fatal(err)
			}
			c, _, err := s.AcceptChatRequest(u, state, "request", ChatRequestInput{Scope: "scope", ThreadID: "thread", Text: "crash fixture"})
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case ChatStarting:
				advanceChat(t, s, u, c, ChatStarting)
			case ChatRunning:
				advanceChat(t, s, u, c, ChatStarting, ChatRunning)
			case ChatCompleted, ChatInterrupted:
				advanceChat(t, s, u, c, ChatStarting, ChatRunning, state)
			case ChatFailed, ChatUncertain:
				advanceChat(t, s, u, c, state)
			case ChatReviewed:
				advanceChat(t, s, u, c, ChatUncertain, ChatReviewed)
			}
		}
		// No Close/checkpoint/cleanup: simulate process loss after committed WAL.
		os.Exit(27)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestChatRequestAbruptExitRecovery$")
	cmd.Env = append(os.Environ(), helperEnv+"="+path)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 27 {
		t.Fatalf("crash fixture did not reach committed exit: %s %v", output, err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatal("fixture did not leave committed WAL", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := s.GetUser("alice")
	if err = s.RecoverChatRequests(); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{ChatAccepted, ChatStarting, ChatRunning, ChatCompleted, ChatFailed, ChatInterrupted, ChatUncertain, ChatReviewed} {
		c, err := s.ChatRequest(u, state, "request")
		if err != nil {
			t.Fatal(err)
		}
		want := state
		if state == ChatAccepted || state == ChatStarting || state == ChatRunning {
			want = ChatUncertain
		}
		if c.State != want || c.Request.Text != "crash fixture" || c.TurnID == "" {
			t.Fatal("restart lost receipt or guessed outcome", c)
		}
		if replay, fresh, err := s.AcceptChatRequest(u, state, "request", *c.Request); err != nil || fresh || !reflect.DeepEqual(c, replay) {
			t.Fatal("restart granted repeat execution", replay, fresh, err)
		}
		if err = s.RecoverChatRequests(); err != nil {
			t.Fatal(err)
		}
		again, err := s.ChatRequest(u, state, "request")
		if err != nil || !reflect.DeepEqual(c, again) {
			t.Fatal("repeated recovery changed result", again, err)
		}
	}
}

func TestVersionElevenChatMigrationPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(db, migrations()[:11]); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db}
	if err = old.CreateUser(User{Name: "alice", PassHash: "synthetic-hash"}); err != nil {
		t.Fatal(err)
	}
	u, _ := old.GetUser("alice")
	creation, err := old.ReserveWorkspaceCreation(u, WorkspaceCreation{RequestID: "creation", Fingerprint: "original", Request: json.RawMessage(`{"source":"empty"}`), Session: Session{ID: "space", User: u.Name, DefaultModel: "frozen-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.CompleteWorkspaceCreation(u, creation.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Grant(u.Name, 12345, "fixture-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	if err = old.InsertUsage(UsageEvent{User: u.Name, SessionID: "space", TurnID: "old-turn", Kind: UsageKindChat, Agent: "codex", Model: "frozen-model", CostMicroUSD: 321}); err != nil {
		t.Fatal(err)
	}
	quota, _ := old.GetQuota(u.Name)
	ledger := old.ListLedger(LedgerFilter{})
	usage := old.ListUsage(UsageFilter{User: u.Name})
	before, err := old.WorkspaceCreation(u, creation.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.WorkspaceCreation(u, creation.RequestID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed creation", after, err)
	}
	newQuota, _ := s.GetQuota(u.Name)
	if !reflect.DeepEqual(quota, newQuota) || !reflect.DeepEqual(ledger, s.ListLedger(LedgerFilter{})) || !reflect.DeepEqual(usage, s.ListUsage(UsageFilter{User: u.Name})) {
		t.Fatal("migration changed accounting")
	}
	newUser, _ := s.GetUser(u.Name)
	if !reflect.DeepEqual(u, newUser) {
		t.Fatal("migration changed user")
	}
	if _, fresh, err := s.AcceptChatRequest(u, "space", "request", ChatRequestInput{Scope: "scope", ThreadID: "thread", Text: "new receipt"}); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
}
