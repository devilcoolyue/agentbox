package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPairTokenRequiresExactAccountCredentialAndCreationIdentity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "synthetic-original-hash"}); err != nil {
		t.Fatal(err)
	}
	original, _ := s.GetUser("alice")
	if ok, err := s.CreateTokenIfUserUnchanged("valid-pair-token", original); err != nil || !ok {
		t.Fatal("unchanged user rejected", ok, err)
	}
	fromToken, ok := s.TokenUser("valid-pair-token")
	if !ok || fromToken.CreatedAt.IsZero() || !fromToken.CreatedAt.Equal(original.CreatedAt) {
		t.Fatal("authenticated request lost account creation identity")
	}
	if err = s.SetPassword("alice", "synthetic-new-hash"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CreateTokenIfUserUnchanged("stale-password-token", original); err != nil || ok {
		t.Fatal("stale password accepted", ok, err)
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&count); err != nil || count != 1 {
		t.Fatal("stale proof created token", count, err)
	}
	if err = s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	// Keep the exact original hash to isolate the creation-identity check from
	// ordinary password changes. A same-name replacement is another account.
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: original.PassHash, CreatedAt: original.CreatedAt.Add(time.Nanosecond)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CreateTokenIfUserUnchanged("replaced-user-token", original); err != nil || ok {
		t.Fatal("replacement account accepted old proof", ok, err)
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&count); err != nil || count != 0 {
		t.Fatal("replacement account received stale token", count, err)
	}
}

func TestPairTokenCannotSurviveConcurrentPasswordResetAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reset, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Close()
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "initial"}); err != nil {
		t.Fatal(err)
	}
	for attempt := range 32 {
		oldHash := fmt.Sprintf("synthetic-old-%d", attempt)
		if err := s.SetPassword("alice", oldHash); err != nil {
			t.Fatal(err)
		}
		proof, _ := s.GetUser("alice")
		token := fmt.Sprintf("racing-pair-token-%d", attempt)
		start := make(chan struct{})
		failures := make(chan error, 2)
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			_, err := s.CreateTokenIfUserUnchanged(token, proof)
			failures <- err
		}()
		go func() {
			defer workers.Done()
			<-start
			_, err := reset.ResetPasswordIfUserUnchanged(proof, fmt.Sprintf("synthetic-new-%d", attempt), "")
			failures <- err
		}()
		close(start)
		workers.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, ok := s.TokenUser(token); ok {
			t.Fatal("old pairing resurrected authentication after completed reset", attempt)
		}
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&count); err != nil || count != 0 {
			t.Fatal("revoked token row remained", count, err)
		}
	}
}

func TestPasswordResetRevokesAtomicallyAndRefusesStaleIdentity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "original"}); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"keep", "revoke"} {
		if err := s.CreateToken(token, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	original, _ := s.GetUser("alice")
	// A revocation failure must roll back the password too. Otherwise a reader
	// could still authenticate a soon-to-be-revoked token with the new hash.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_fixture_revocation BEFORE DELETE ON tokens BEGIN SELECT RAISE(ABORT, 'synthetic revocation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResetPasswordIfUserUnchanged(original, "changed", "keep"); err == nil || ok {
		t.Fatal("revocation failure did not fail the entire reset", ok, err)
	}
	for _, token := range []string{"keep", "revoke"} {
		user, ok := s.TokenUser(token)
		if !ok || user.PassHash != original.PassHash {
			t.Fatal("failed reset exposed a partial password/token state")
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_fixture_revocation"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResetPasswordIfUserUnchanged(original, "changed", "keep"); err != nil || !ok {
		t.Fatal("reset failed", ok, err)
	}
	if kept, ok := s.TokenUser("keep"); !ok || kept.PassHash != "changed" {
		t.Fatal("current token did not survive self reset")
	}
	if _, ok := s.TokenUser("revoke"); ok {
		t.Fatal("other token survived reset")
	}
	if ok, err := s.ResetPasswordIfUserUnchanged(original, "stale-overwrite", ""); err != nil || ok {
		t.Fatal("stale password request changed current credentials", ok, err)
	}
	current, _ := s.GetUser("alice")
	if current.PassHash != "changed" {
		t.Fatal("stale reset changed password")
	}
	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: current.PassHash, CreatedAt: current.CreatedAt.Add(time.Nanosecond)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResetPasswordIfUserUnchanged(current, "stale-overwrite", ""); err != nil || ok {
		t.Fatal("stale reset changed a replacement account", ok, err)
	}
}

func TestPasswordResetReadersNeverSeeNewCredentialWithRevokedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "original"}); err != nil {
		t.Fatal(err)
	}
	for attempt := range 32 {
		proof, _ := s.GetUser("alice")
		if err := s.CreateToken("revoked-token", "alice"); err != nil {
			t.Fatal(err)
		}
		ready, done, observed := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
		go func() {
			close(ready)
			for {
				if user, ok := reader.TokenUser("revoked-token"); ok && user.PassHash != proof.PassHash {
					observed <- true
					return
				}
				select {
				case <-done:
					observed <- false
					return
				default:
				}
			}
		}()
		<-ready
		changed, resetErr := s.ResetPasswordIfUserUnchanged(proof, fmt.Sprintf("changed-%d", attempt), "")
		close(done)
		partial := <-observed
		if resetErr != nil || !changed || partial {
			t.Fatal("password reset exposed an intermediate credential state", changed, resetErr, partial)
		}
		if _, ok := reader.TokenUser("revoked-token"); ok {
			t.Fatal("completed reset left token valid")
		}
	}
}
