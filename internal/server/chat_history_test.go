package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func testThreadServer(t *testing.T) (*Server, store.Session) {
	t.Helper()
	s := &Server{cfg: &config.Config{DataDir: t.TempDir()}}
	sess := store.Session{ID: "abc123", User: "u1"}
	if err := os.MkdirAll(s.sessionDir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	return s, sess
}

// 旧版单文件 chat.jsonl 按 divider 切段迁移成线程文件：每段一个线程、
// divider 不保留、从落盘事件里反挖 provider 会话 id 供续聊、空段丢弃、
// 最后一段成为活跃线程，原文件改名备份。
func TestMigrateThreadsFromLegacy(t *testing.T) {
	s, sess := testThreadServer(t)
	log := strings.Join([]string{
		`{"ts":"2026-07-01T10:00:00Z","kind":"user","text":"第一段问题"}`,
		`{"ts":"2026-07-01T10:00:05Z","kind":"event","event":{"type":"system","session_id":"sid-1a"}}`,
		`{"ts":"2026-07-01T10:00:09Z","kind":"event","event":{"type":"result","session_id":"sid-1b"}}`,
		`{"ts":"2026-07-02T09:00:00Z","kind":"divider"}`,
		`{"ts":"2026-07-02T09:00:01Z","kind":"user","text":"第二段问题"}`,
		`{"ts":"2026-07-03T08:00:00Z","kind":"divider"}`, // 双重置：空段
		`{"ts":"2026-07-04T08:00:00Z","kind":"divider"}`,
		`{"ts":"2026-07-04T08:00:01Z","kind":"user","text":"当前段"}`,
		`{"ts":"2026-07-04T08:00:05Z","kind":"event","event":{"msg":{"type":"session_configured","session_id":"sid-3"}}}`,
		``,
		`not json`,
	}, "\n")
	if err := os.WriteFile(s.chatLogPath(sess), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.migrateThreads(sess); err != nil {
		t.Fatal(err)
	}
	// 幂等：已迁移过直接返回
	if err := s.migrateThreads(sess); err != nil {
		t.Fatal(err)
	}

	threads := s.listThreads(sess)
	if len(threads) != 3 {
		t.Fatalf("threads = %d, want 3: %+v", len(threads), threads)
	}
	// listThreads 按最近更新倒序
	if threads[0].Title != "当前段" || threads[2].Title != "第一段问题" {
		t.Fatalf("thread order unexpected: %+v", threads)
	}
	if !threads[0].Resumable || !threads[2].Resumable {
		t.Fatalf("threads with recorded ids should be resumable: %+v", threads)
	}
	if threads[1].Resumable {
		t.Fatalf("thread without any session id must not be resumable: %+v", threads[1])
	}

	// 活跃线程 = 最后一段；resume id 取该段最后出现的 id
	active := s.activeThread(sess)
	if active != threads[0].ID {
		t.Fatalf("active = %q, want %q", active, threads[0].ID)
	}
	if got := s.threadResumeID(sess, active); got != "sid-3" {
		t.Fatalf("resume id = %q, want sid-3", got)
	}
	if got := s.threadResumeID(sess, threads[2].ID); got != "sid-1b" {
		t.Fatalf("resume id = %q, want sid-1b (last id in segment)", got)
	}

	// 原文件已备份
	if _, err := os.Stat(s.chatLogPath(sess)); !os.IsNotExist(err) {
		t.Fatalf("legacy chat.jsonl should be renamed away")
	}
	if _, err := os.Stat(s.chatLogPath(sess) + ".bak"); err != nil {
		t.Fatalf("legacy backup missing: %v", err)
	}
	// 线程文件里不再有 divider 行
	raw, err := os.ReadFile(s.threadPath(sess, active))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"divider"`) {
		t.Fatalf("thread transcript should not carry dividers: %s", raw)
	}
}

// 没有旧文件时迁移只建目录；ensureActiveThread 为首条消息铸出线程 id。
func TestEnsureActiveThread(t *testing.T) {
	s, sess := testThreadServer(t)
	if err := s.migrateThreads(sess); err != nil {
		t.Fatal(err)
	}
	if got := s.activeThread(sess); got != "" {
		t.Fatalf("fresh session active = %q, want empty", got)
	}
	id, err := s.ensureActiveThread(sess)
	if err != nil {
		t.Fatal(err)
	}
	if !threadIDRe.MatchString(id) {
		t.Fatalf("minted id %q does not match threadIDRe", id)
	}
	id2, err := s.ensureActiveThread(sess)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Fatalf("ensureActiveThread not stable: %q then %q", id, id2)
	}
}

// scanThread 元数据：标题=首条用户消息、轮数、时间范围与可续聊标记。
func TestScanThread(t *testing.T) {
	s, sess := testThreadServer(t)
	if err := os.MkdirAll(s.chatsDir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		`{"ts":"2026-07-10T10:00:00Z","kind":"user","text":"问个问题"}`,
		`{"ts":"2026-07-10T10:00:03Z","kind":"event","event":{"type":"assistant"}}`,
		`{"ts":"2026-07-10T10:00:04Z","kind":"chat_session","text":"sid-9"}`,
		`{"ts":"2026-07-10T10:05:00Z","kind":"user","text":"追问"}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(s.chatsDir(sess), "t1.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, ok := s.scanThread(sess, "t1")
	if !ok {
		t.Fatal("scanThread failed")
	}
	if m.Title != "问个问题" || m.Turns != 2 || !m.Resumable {
		t.Fatalf("meta unexpected: %+v", m)
	}
	if !m.Updated.After(m.TS) {
		t.Fatalf("updated %v should be after ts %v", m.Updated, m.TS)
	}
}

func TestTruncRunes(t *testing.T) {
	if got := truncRunes("你好世界", 2); got != "你好…" {
		t.Fatalf("truncRunes = %q", got)
	}
	if got := truncRunes("short", 120); got != "short" {
		t.Fatalf("truncRunes = %q", got)
	}
}
