package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanExpiredImagesKeepsReferenced(t *testing.T) {
	s, sess := newTestServer(t)

	shared := filepath.Join(s.cfg.DataDir, "users", sess.User, "shared", imagesSubdir)
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, age time.Duration) string {
		p := filepath.Join(shared, name)
		if err := os.WriteFile(p, []byte("img"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	referenced := write("20260101-000000-aaa111.png", 72*time.Hour) // 过期但被引用
	orphan := write("20260101-000000-bbb222.png", 72*time.Hour)     // 过期且无人引用
	fresh := write("20260101-000000-ccc333.png", time.Hour)         // 未过期

	// 一条引用了 referenced 的对话记录
	chats := filepath.Join(s.sessionDir(sess), "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := `{"ts":"2026-01-01T00:00:00Z","kind":"user","text":"看看这个 [图片#1 /shared/.images/20260101-000000-aaa111.png]"}` + "\n"
	if err := os.WriteFile(filepath.Join(chats, "t1.jsonl"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	s.cleanExpiredImages()

	if _, err := os.Stat(referenced); err != nil {
		t.Fatalf("referenced attachment was deleted: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("unreferenced expired attachment survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh attachment was deleted: %v", err)
	}
}
