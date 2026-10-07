package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func creationFixture(t *testing.T) (*Store, User, WorkspaceCreation) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetUser("alice")
	c := WorkspaceCreation{RequestID: "request", Fingerprint: "fingerprint", Request: json.RawMessage(`{"name":"project"}`), Session: Session{ID: "space", User: u.Name, Name: "project", Agent: "codex", AccountID: "account", DefaultModel: "original-model", Status: StatusStopped, CreatedAt: time.Now()}}
	return s, u, c
}

func TestCreationReservationConcurrentReplayAndIdentity(t *testing.T) {
	s, u, c := creationFixture(t)
	var wg sync.WaitGroup
	results := make(chan WorkspaceCreation, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidate := c
			candidate.Session.ID = NewID()
			r, err := s.ReserveWorkspaceCreation(u, candidate)
			if err != nil {
				t.Error(err)
				return
			}
			results <- r
		}(i)
	}
	wg.Wait()
	close(results)
	id := ""
	for r := range results {
		if id == "" {
			id = r.Session.ID
		}
		if r.Session.ID != id {
			t.Fatal("request allocated more than one space")
		}
	}
	if len(s.All()) != 0 {
		t.Fatal("reservation published incomplete session")
	}
	c.Fingerprint = "changed"
	if _, err := s.ReserveWorkspaceCreation(u, c); !errors.Is(err, ErrCreationConflict) {
		t.Fatal("changed payload accepted", err)
	}
	first, err := s.CompleteWorkspaceCreation(u, c.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != id || first.DefaultModel != "original-model" || len(s.All()) != 1 {
		t.Fatal("snapshot/session mismatch")
	}
	if _, err = s.CompleteWorkspaceCreation(u, c.RequestID); !errors.Is(err, ErrCreationConflict) {
		t.Fatal("completion duplicated")
	}
	if err = s.Delete(id); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.WorkspaceCreation(u, c.RequestID)
	if err != nil || receipt.State != "abandoned" {
		t.Fatal("deletion lost tombstone")
	}
	if _, err = s.CompleteWorkspaceCreation(u, c.RequestID); !errors.Is(err, ErrCreationConflict) {
		t.Fatal("deleted space resurrected")
	}
	old := u
	if err = s.DeleteUser(u.Name); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser(User{Name: u.Name, Role: RoleUser, CreatedAt: u.CreatedAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUser(u.Name)
	if _, err = s.WorkspaceCreation(u, c.RequestID); err == nil {
		t.Fatal("replacement user inherited receipt")
	}
	c.Fingerprint = "fingerprint"
	if _, err = s.ReserveWorkspaceCreation(old, c); err == nil {
		t.Fatal("deleted identity reserved work")
	}
}

func TestCreationCommitRollsBackSessionWhenReceiptCannotAdvance(t *testing.T) {
	s, u, c := creationFixture(t)
	if _, err := s.ReserveWorkspaceCreation(u, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fixture_receipt_failure BEFORE UPDATE ON workspace_creations BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteWorkspaceCreation(u, c.RequestID); err == nil {
		t.Fatal("failed receipt commit succeeded")
	}
	if len(s.All()) != 0 {
		t.Fatal("session published without receipt")
	}
	r, err := s.WorkspaceCreation(u, c.RequestID)
	if err != nil || r.State != "reserved" {
		t.Fatal("failed transaction advanced receipt")
	}
}

func TestImportReceiptsDoNotReplayOrHideUnknownResults(t *testing.T) {
	s, u, c := creationFixture(t)
	if _, err := s.ReserveWorkspaceCreation(u, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteWorkspaceCreation(u, c.RequestID); err != nil {
		t.Fatal(err)
	}
	one := WorkspaceImport{AttemptID: "one", Kind: "upload", Directory: "project", Fingerprint: "bytes-one"}
	if _, fresh, err := s.BeginWorkspaceImport(u, c.RequestID, one); err != nil || !fresh {
		t.Fatal(err)
	}
	if r, fresh, err := s.BeginWorkspaceImport(u, c.RequestID, one); err != nil || fresh || r.State != "running" {
		t.Fatal("duplicate import ran")
	}
	changed := one
	changed.Fingerprint = "different"
	if _, _, err := s.BeginWorkspaceImport(u, c.RequestID, changed); !errors.Is(err, ErrCreationConflict) {
		t.Fatal("new content reused import ID")
	}
	two := one
	two.AttemptID = "two"
	if _, _, err := s.BeginWorkspaceImport(u, c.RequestID, two); !errors.Is(err, ErrImportPending) {
		t.Fatal("concurrent second import admitted")
	}
	var seq int
	var databaseName, databasePath string
	if err := s.db.QueryRow("PRAGMA database_list").Scan(&seq, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = reopened
	if err := s.RecoverWorkspaceImports(); err != nil {
		t.Fatal(err)
	}
	r, err := s.WorkspaceImport(u, c.RequestID, one.AttemptID)
	if err != nil || r.State != "uncertain" {
		t.Fatal("restart pretended failure/success")
	}
	if err := s.FinishWorkspaceCreation(u, c.RequestID, false); !errors.Is(err, ErrImportPending) {
		t.Fatal("unknown import silently completed")
	}
	if _, _, err := s.BeginWorkspaceImport(u, c.RequestID, two); !errors.Is(err, ErrImportPending) {
		t.Fatal("unknown import automatically retried")
	}
	if err := s.ReviewWorkspaceImport(u, c.RequestID, one.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.BeginWorkspaceImport(u, c.RequestID, two); err != nil || !fresh {
		t.Fatal(err)
	}
	if err := s.FinishWorkspaceImport(u, c.RequestID, two.AttemptID, "succeeded", json.RawMessage(`{"directory":"project"}`)); err != nil {
		t.Fatal(err)
	}
	if r, fresh, err := s.BeginWorkspaceImport(u, c.RequestID, two); err != nil || fresh || r.State != "succeeded" {
		t.Fatal("successful import replayed")
	}
	if err := s.FinishWorkspaceCreation(u, c.RequestID, false); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.PendingWorkspaceCreations(u); err != nil || len(rows) != 0 {
		t.Fatal("finished flow remains pending")
	}
}

func TestCreationAbandonFencesDelayedUnknownRequest(t *testing.T) {
	s, u, c := creationFixture(t)
	if err := s.AbandonUnknownWorkspaceCreation(u, c.RequestID, "unused-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveWorkspaceCreation(u, c); !errors.Is(err, ErrCreationConflict) {
		t.Fatal("delayed request created a space after abandonment", err)
	}
	if len(s.All()) != 0 {
		t.Fatal("abandonment created workspace")
	}
}

func TestCreationCompletionRechecksGitAccessInTransaction(t *testing.T) {
	s, u, c := creationFixture(t)
	connection, err := s.SaveGitConnection(GitConnection{ID: "git", Owner: u.Name, Label: "fixture", Provider: "gitlab", BaseURL: "https://git.example.invalid", AuthType: "pat", Secret: []byte("synthetic"), Enabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.GitConnectionID = connection.ID
	if _, err = s.ReserveWorkspaceCreation(u, c); err != nil {
		t.Fatal(err)
	}
	connection.Enabled = false
	connection.Secret = []byte("synthetic")
	if _, err = s.SaveGitConnection(connection, connection.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteWorkspaceCreation(u, c.RequestID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("revoked Git connection published", err)
	}
	if len(s.All()) != 0 {
		t.Fatal("session published before Git access check")
	}
}

func TestVersionTenCreationMigrationPreservesWorkspaceAndLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(db, migrations()[:10]); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db}
	if err = old.CreateUser(User{Name: "alice", Role: RoleAdmin, PassHash: "synthetic-hash"}); err != nil {
		t.Fatal(err)
	}
	if err = old.Put(Session{ID: "legacy", User: "alice", Agent: "claude", DefaultModel: "saved-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Grant("alice", 12345, "fixture-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	before, _ := old.GetQuota("alice")
	ledger := old.ListLedger(LedgerFilter{})
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	after, _ := upgraded.GetQuota("alice")
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("migration changed quota")
	}
	a, _ = json.Marshal(ledger)
	b, _ = json.Marshal(upgraded.ListLedger(LedgerFilter{}))
	if string(a) != string(b) {
		t.Fatal("migration changed ledger")
	}
	user, _ := upgraded.GetUser("alice")
	sess, _ := upgraded.Get("legacy")
	if user.PassHash != "synthetic-hash" || sess.DefaultModel != "saved-model" {
		t.Fatal("migration changed credentials/model")
	}
	var version int
	if err = upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("migration version not recorded")
	}
}
