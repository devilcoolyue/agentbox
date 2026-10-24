package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
)

// 终端用量把余额扣穿后，用户每个运行中的空间都要停一次终端里的 Agent；停过的
// 空间短时间内不重复 exec，余额还够、没开额度或空间没在跑的都不动。
func TestTerminalUsageExhaustingQuotaStopsTerminalAgents(t *testing.T) {
	s, sess := newTestServer(t)
	stopped := make(chan string, 8)
	s.quotaStops.run = func(_ context.Context, containerID, script string) (string, error) {
		if !strings.Contains(script, "kill -TERM") || !strings.Contains(script, "额度已用完") {
			t.Errorf("script = %q", script)
		}
		stopped <- containerID
		return "123\n", nil
	}
	s.store.OnUsageWritten(s.noteUsageForQuota)
	sess.Status, sess.ContainerID = store.StatusRunning, "c-running"
	idle := store.Session{ID: "s2", User: sess.User, Name: "idle", Status: store.StatusStopped}
	other := store.Session{ID: "s3", User: "bob", Name: "bob", Status: store.StatusRunning, ContainerID: "c-bob"}
	for _, x := range []store.Session{sess, idle, other} {
		if err := s.store.Put(x); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.store.Grant(sess.User, 1000, "grant:seed", "", "root"); err != nil {
		t.Fatal(err)
	}
	usage := func(user, req string, cost int64) {
		t.Helper()
		if err := s.store.UpsertTerminalUsage(store.UsageEvent{User: user, SessionID: "s1", TurnID: req, ReqID: req,
			Agent: "claude", Model: "claude-opus-5-5", OutputTokens: cost, CostMicroUSD: cost}); err != nil {
			t.Fatal(err)
		}
	}
	expectNone := func() {
		t.Helper()
		select {
		case c := <-stopped:
			t.Fatalf("unexpected stop in %s", c)
		case <-time.After(100 * time.Millisecond):
		}
	}

	usage(sess.User, "req_1", 400) // 余额还剩 600
	usage("bob", "req_bob", 5000)  // bob 没开额度
	expectNone()

	usage(sess.User, "req_2", 700) // 扣穿
	select {
	case c := <-stopped:
		if c != "c-running" {
			t.Fatalf("stopped %s", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal agents not stopped")
	}
	usage(sess.User, "req_3", 10) // 停过不久，不再 exec
	expectNone()
}

// 只停终端（tmux / 后台）里的进程：脚本按祖先是否落到 docker-init 判断，网页回合
// 由 docker exec 拉起、祖先的父进程号是 0，不能被它误杀。
func TestTerminalAgentStopScriptSparesExecRoots(t *testing.T) {
	script := terminalAgentStopScript("额度已用完")
	for _, want := range []string{"0) top=exec", "1) top=init", `[ "$top" = init ] || continue`, "claude|claude.exe|codex"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}
