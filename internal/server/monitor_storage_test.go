package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbox/internal/storagepolicy"
	"agentbox/internal/store"
)

// 目录统计按工作空间与共享目录拆开；配额按数据根目录与缓存 owner key 归到用户，
// 账号删了配额还在的也列出来并计入已分配。
func TestMonitorStorageView(t *testing.T) {
	s, _ := newTestServer(t)
	for _, name := range []string{"alice", "bob"} {
		if err := s.store.CreateUser(store.User{Name: name, Role: "user", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string, n int) {
		path := filepath.Join(s.cfg.DataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("users/alice/sessions/s1/home/.bashrc", 100)
	write("users/alice/sessions/s1/workspace/a.txt", 20)
	write("users/alice/sessions/s2/chats/t.jsonl", 3)
	write("users/alice/shared/video.mp4", 1000)
	report := measureStorage(context.Background(), s.cfg.DataDir)
	if report.Sessions["s1"] != 120 || report.Sessions["s2"] != 3 || report.Shared["alice"] != 1000 || report.Users["alice"] != 1123 {
		t.Fatalf("report = sessions %v shared %v users %v", report.Sessions, report.Shared, report.Users)
	}

	bob, err := s.storageOwner(s.store.ListUsers()[1])
	if err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(s.cfg.DataDir, "users")
	s.storage.report = report
	s.storage.quotaAt = 1
	s.storage.quota = storagepolicy.Report{Projects: []storagepolicy.Project{
		{Kind: "data", Root: filepath.Join(users, "alice"), Usage: storagepolicy.Usage{UsedBytes: 4096, LimitBytes: 1 << 30}},
		{Kind: "cache", Owner: bob, Root: "/tools/private/" + bob, Usage: storagepolicy.Usage{UsedBytes: 7, LimitBytes: 1 << 20}},
		{Kind: "data", Root: filepath.Join(users, "gone"), Usage: storagepolicy.Usage{LimitBytes: 1 << 20}},
		{Kind: "cache", Owner: "deadbeefcafe", Root: "/tools/private/deadbeefcafe", Usage: storagepolicy.Usage{LimitBytes: 1 << 20}},
		// 删除时已释放上限：只按留下的文件计入
		{Kind: "data", Root: filepath.Join(users, "left"), Retired: true, Usage: storagepolicy.Usage{UsedBytes: 5 << 20}},
	}}
	view := s.monitorStorageView(map[string]int{"alice": 2})
	if view.Allocated != 1<<30+8<<20 {
		t.Fatalf("allocated = %d", view.Allocated)
	}
	got := map[string]userStorage{}
	for _, u := range view.Users {
		got[u.User] = u
	}
	if a := got["alice"]; a.Data == nil || a.Data.UsedBytes != 4096 || a.Files != 1123 || a.Shared != 1000 || a.Sessions != 2 || a.Deleted {
		t.Fatalf("alice = %+v", a)
	}
	if b := got["bob"]; b.Cache == nil || b.Cache.UsedBytes != 7 || b.Data != nil || b.Deleted {
		t.Fatalf("bob = %+v", b)
	}
	if !got["gone"].Deleted || got["gone"].Released || !got["cache:deadbeef"].Deleted || !got["left"].Deleted || !got["left"].Released {
		t.Fatalf("deleted users missing: %+v", view.Users)
	}
	if last := view.Users[len(view.Users)-1]; !last.Deleted {
		t.Fatalf("deleted users should sort last: %+v", view.Users)
	}
}
