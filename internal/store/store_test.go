package store

import (
	"database/sql"
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

func TestStopReasonRoundTripAndMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	// A database created before stop_reason existed: the legacy schema, minus
	// the column migrate() adds.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, user TEXT NOT NULL, name TEXT NOT NULL,
		agent TEXT NOT NULL, account_id TEXT NOT NULL,
		container_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
		chat_session TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO sessions
		(id,user,name,agent,account_id,status,created_at,updated_at)
		VALUES ('old','alice','legacy','claude','acct','stopped',?,?)`,
		time.Now().Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	st, err := Open(path) // must migrate rather than fail
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer st.Close()

	got, ok := st.Get("old")
	if !ok || got.StopReason != "" {
		t.Fatalf("migrated row = %+v, ok=%v; want empty stop reason", got, ok)
	}

	if _, err := st.Update("old", func(s *Session) { s.StopReason = StopIdle }); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get("old"); got.StopReason != StopIdle {
		t.Fatalf("stop reason = %q, want %q", got.StopReason, StopIdle)
	}
}

func TestUsageRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Truncate(time.Millisecond)
	// 一个回合按模型拆成两行，共享 turn_id。
	if err := s.InsertUsage(
		UsageEvent{TS: now, User: "alice", SessionID: "s1", ThreadID: "t1", TurnID: "turn1",
			Agent: "claude", AccountID: "a1", Model: "claude-haiku-4-5-20251001",
			InputTokens: 531, OutputTokens: 18, CostMicroUSD: 621, DurationMS: 19788,
			Raw: `{"type":"result"}`},
		UsageEvent{TS: now, User: "alice", SessionID: "s1", ThreadID: "t1", TurnID: "turn1",
			Agent: "claude", AccountID: "a1", Model: "claude-opus-4-8",
			InputTokens: 2, OutputTokens: 1870, CacheReadTokens: 7342, CacheWriteTokens: 5265,
			CostMicroUSD: 103081, DurationMS: 19788},
	); err != nil {
		t.Fatal(err)
	}
	// 另一个用户，用来验证过滤没串号。
	if err := s.InsertUsage(UsageEvent{TS: now.Add(time.Second), User: "bob", SessionID: "s2",
		TurnID: "turn2", Agent: "codex", InputTokens: 13109, CacheReadTokens: 3840, OutputTokens: 14}); err != nil {
		t.Fatal(err)
	}

	all := s.ListUsage(UsageFilter{})
	if len(all) != 3 {
		t.Fatalf("总行数 = %d, want 3", len(all))
	}

	mine := s.ListUsage(UsageFilter{User: "alice"})
	if len(mine) != 2 {
		t.Fatalf("alice 的行数 = %d, want 2", len(mine))
	}
	var cost int64
	for _, e := range mine {
		if e.TurnID != "turn1" {
			t.Errorf("turn_id 串了: %+v", e)
		}
		cost += e.CostMicroUSD
	}
	if cost != 103702 {
		t.Errorf("alice 费用合计 = %d 微美元, want 103702", cost)
	}

	// 时间过滤：Until 是开区间，不应带上 bob 那条。
	early := s.ListUsage(UsageFilter{Until: now.Add(time.Second)})
	if len(early) != 2 {
		t.Fatalf("Until 过滤后 = %d 行, want 2", len(early))
	}

	// Asc 翻转整个结果集，不是只翻当前这一页：带 Limit 时取的必须是最早的那几行，
	// 否则前端切到正序后翻页会在同一批「最新的行」里打转。
	ids := func(evs []UsageEvent) []int64 {
		out := make([]int64, len(evs))
		for i, e := range evs {
			out[i] = e.ID
		}
		return out
	}
	asc := s.ListUsage(UsageFilter{Asc: true})
	if len(asc) != 3 || asc[0].ID != all[2].ID || asc[2].ID != all[0].ID {
		t.Fatalf("Asc 没把顺序翻过来: %v", ids(asc))
	}
	if first := s.ListUsage(UsageFilter{Asc: true, Limit: 1}); len(first) != 1 || first[0].ID != asc[0].ID {
		t.Errorf("Asc + Limit 取到 %v, want 最早的那行 id=%d", ids(first), asc[0].ID)
	}

	// 字段完整往返（含 raw 与缓存分桶）。
	one := s.ListUsage(UsageFilter{User: "alice", Limit: 1})
	if len(one) != 1 {
		t.Fatalf("Limit 未生效: %d 行", len(one))
	}
	got := s.ListUsage(UsageFilter{SessionID: "s1"})
	var opus UsageEvent
	for _, e := range got {
		if e.Model == "claude-opus-4-8" {
			opus = e
		}
	}
	if opus.CacheReadTokens != 7342 || opus.CacheWriteTokens != 5265 || opus.DurationMS != 19788 {
		t.Errorf("opus 行往返丢字段: %+v", opus)
	}
	if !opus.TS.Equal(now) {
		t.Errorf("时间戳往返不一致: got %v want %v", opus.TS, now)
	}
}

// kind 把用户自己的对话和服务端自动起标题的消耗分开——两者都可能落在同一个
// 便宜模型上，只看 model 区分不了。
func TestUsageKindFilter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertUsage(
		UsageEvent{User: "alice", SessionID: "s1", TurnID: "t1", Agent: "claude",
			Model: "claude-opus-4-8", Kind: UsageKindChat, CostMicroUSD: 103081},
		UsageEvent{User: "alice", SessionID: "s1", TurnID: "t2", Agent: "claude",
			Model: "claude-haiku-4-5-20251001", Kind: UsageKindTitle, CostMicroUSD: 621},
		// kind 留空的旧行应落成 chat，而不是空串。
		UsageEvent{User: "alice", SessionID: "s1", TurnID: "t3", Agent: "claude", CostMicroUSD: 1},
	); err != nil {
		t.Fatal(err)
	}

	chat := s.ListUsage(UsageFilter{User: "alice", Kind: UsageKindChat})
	if len(chat) != 2 {
		t.Fatalf("chat 行数 = %d, want 2（含 kind 留空补默认的那行）", len(chat))
	}
	titles := s.ListUsage(UsageFilter{User: "alice", Kind: UsageKindTitle})
	if len(titles) != 1 || titles[0].CostMicroUSD != 621 {
		t.Fatalf("title 行 = %+v", titles)
	}
	if titles[0].Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model 往返丢失: %q", titles[0].Model)
	}
}
