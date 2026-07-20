package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"sessions":{"aaa111":{"id":"aaa111","user":"default","name":"联调测试","agent":"claude","account_id":"claude-1","container_id":"c1","status":"running","chat_session":"chat-1","created_at":"2026-07-16T19:20:50.494573026-07:00","updated_at":"2026-07-18T00:43:54.141040221-07:00"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sess, ok := s.Get("aaa111")
	if !ok {
		t.Fatal("migrated session not found")
	}
	if sess.Name != "联调测试" || sess.Agent != "claude" || sess.ChatSession != "chat-1" {
		t.Fatalf("migrated session mismatch: %+v", sess)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-07-16T19:20:50.494573026-07:00")
	if !sess.CreatedAt.Equal(want) {
		t.Fatalf("created_at mismatch: got %v want %v", sess.CreatedAt, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Fatal("legacy state.json should be renamed after migration")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.migrated")); err != nil {
		t.Fatalf("state.json.migrated missing: %v", err)
	}
}

func TestCRUD(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	for _, sess := range []Session{
		{ID: "one", User: "u1", Name: "first", Agent: "claude", AccountID: "a", Status: StatusStopped, CreatedAt: now.Add(-time.Hour)},
		{ID: "two", User: "u1", Name: "second", Agent: "codex", AccountID: "b", Status: StatusRunning, CreatedAt: now},
		{ID: "other", User: "u2", Name: "theirs", Agent: "claude", AccountID: "a", Status: StatusStopped, CreatedAt: now},
	} {
		if err := s.Put(sess); err != nil {
			t.Fatal(err)
		}
	}

	list := s.List("u1")
	if len(list) != 2 || list[0].ID != "two" || list[1].ID != "one" {
		t.Fatalf("List order/content wrong: %+v", list)
	}
	if all := s.All(); len(all) != 3 {
		t.Fatalf("All returned %d sessions, want 3", len(all))
	}

	updated, err := s.Update("one", func(sess *Session) {
		sess.Status = StatusRunning
		sess.ContainerID = "cid"
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusRunning || updated.ContainerID != "cid" {
		t.Fatalf("Update result wrong: %+v", updated)
	}
	got, _ := s.Get("one")
	if got.Status != StatusRunning || got.ContainerID != "cid" {
		t.Fatalf("Update not persisted: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt not set")
	}

	if _, err := s.Update("missing", func(*Session) {}); err == nil {
		t.Fatal("Update of missing session should error")
	}

	if err := s.Delete("two"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("two"); ok {
		t.Fatal("deleted session still present")
	}
}

func TestUsersAndTokens(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if n := s.CountUsers(); n != 0 {
		t.Fatalf("CountUsers = %d, want 0", n)
	}
	if err := s.CreateUser(User{Name: "boxadmin", Role: RoleAdmin, PassHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "h2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "dup"}); err == nil {
		t.Fatal("duplicate user should error")
	}
	if n := s.CountUsers(); n != 2 {
		t.Fatalf("CountUsers = %d, want 2", n)
	}
	u, ok := s.GetUser("alice")
	if !ok || u.Role != RoleUser || u.PassHash != "h2" {
		t.Fatalf("GetUser mismatch: %+v ok=%v", u, ok)
	}

	// 令牌签发与解析
	for _, tok := range []string{"t1", "t2"} {
		if err := s.CreateToken(tok, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	if u, ok := s.TokenUser("t1"); !ok || u.Name != "alice" {
		t.Fatalf("TokenUser(t1) = %+v ok=%v", u, ok)
	}
	if _, ok := s.TokenUser("nope"); ok {
		t.Fatal("unknown token should not resolve")
	}

	// 改密后吊销其他端令牌，保留当前端
	if err := s.SetPassword("alice", "h3"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserTokensExcept("alice", "t2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.TokenUser("t1"); ok {
		t.Fatal("t1 should be revoked")
	}
	if _, ok := s.TokenUser("t2"); !ok {
		t.Fatal("t2 should survive")
	}

	// 会话归属迁移与按用户计数
	if err := s.Put(Session{ID: "s1", User: "default", Name: "n", Agent: "claude", AccountID: "a", Status: StatusStopped, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReassignUser("default", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("s1"); got.User != "boxadmin" {
		t.Fatalf("ReassignUser: session user = %q", got.User)
	}
	if counts := s.SessionCounts(); counts["boxadmin"] != 1 {
		t.Fatalf("SessionCounts = %v", counts)
	}

	// 删除用户连同令牌
	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetUser("alice"); ok {
		t.Fatal("deleted user still present")
	}
	if _, ok := s.TokenUser("t2"); ok {
		t.Fatal("tokens of deleted user should be gone")
	}
}

func TestOpenWithoutLegacy(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Reopen: schema init and legacy check must be idempotent.
	s, err = Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if all := s.All(); len(all) != 0 {
		t.Fatalf("expected empty store, got %+v", all)
	}
}
