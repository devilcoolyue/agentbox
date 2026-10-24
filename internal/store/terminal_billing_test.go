package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func terminalUsage(user, req string, out, cost int64) UsageEvent {
	return UsageEvent{
		User: user, SessionID: "s1", TurnID: req, ReqID: req,
		Agent: "claude", Model: "claude-opus-5-5",
		InputTokens: 10, OutputTokens: out, CostMicroUSD: cost,
	}
}

func ledgerRefs(t *testing.T, s *Store, user string) map[string]int64 {
	t.Helper()
	refs := map[string]int64{}
	for _, e := range s.ListLedger(LedgerFilter{User: user, Reason: ReasonSpend}) {
		refs[e.Ref] = e.DeltaMicroUSD
	}
	return refs
}

// 同一请求越写越长：先按全额扣 usage:<id>，之后每次只扣增长的部分，键依次是
// usage:<id>:2、:3；重扫不变的行不动账；余额与账本重算一致。
func TestTerminalChargesGrowthWithDistinctRefs(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 1_000_000, "grant:seed", "", "root"); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ out, cost int64 }{{100, 1000}, {100, 1000}, {150, 1500}, {150, 1500}, {200, 2100}} {
		if err := s.UpsertTerminalUsage(terminalUsage("alice", "req_A", step.out, step.cost)); err != nil {
			t.Fatal(err)
		}
	}
	rows := s.ListUsage(UsageFilter{User: "alice"})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	id := rows[0].ID
	want := map[string]int64{
		usageRef(id): -1000, usageRef(id) + ":2": -500, usageRef(id) + ":3": -600,
	}
	got := ledgerRefs(t, s, "alice")
	if len(got) != len(want) {
		t.Fatalf("ledger = %v, want %v", got, want)
	}
	for ref, d := range want {
		if got[ref] != d {
			t.Fatalf("ledger = %v, want %v", got, want)
		}
	}
	q, _ := s.GetQuota("alice")
	if q.BalanceMicroUSD != 1_000_000-2100 || q.SpentMicroUSD != 2100 {
		t.Fatalf("quota = %+v", q)
	}
	if cached, fromLedger, err := s.RecomputeBalance("alice"); err != nil || cached != fromLedger {
		t.Fatalf("cached %d, ledger %d, %v", cached, fromLedger, err)
	}
}

func usageRef(id int64) string { return fmt.Sprintf("usage:%d", id) }

// 没开额度时记下的用量算已结清：之后管理员开了额度、重扫把每一行重写一遍，
// 也只扣开额度之后增长的那部分，不把以前的消耗追扣回来。
func TestTerminalUsageBeforeQuotaIsNeverCharged(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertTerminalUsage(terminalUsage("alice", "req_A", 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant("alice", 1_000_000, "grant:seed", "", "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTerminalUsage(terminalUsage("alice", "req_A", 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if got := ledgerRefs(t, s, "alice"); len(got) != 0 {
		t.Fatalf("rescan charged history: %v", got)
	}
	if err := s.UpsertTerminalUsage(terminalUsage("alice", "req_A", 130, 1300)); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.GetQuota("alice"); q.BalanceMicroUSD != 1_000_000-300 {
		t.Fatalf("balance = %d, want only the growth charged", q.BalanceMicroUSD)
	}
}

// 启用终端计费之前的请求（扫描器第一次读到的旧日志）只记不扣；费用变少不退。
func TestTerminalUsageBeforeBillingAndShrinkingCost(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 1_000_000, "grant:seed", "", "root"); err != nil {
		t.Fatal(err)
	}
	old := terminalUsage("alice", "req_old", 100, 1000)
	old.TS = time.Now().Add(-time.Hour)
	if _, err := s.db.Exec("UPDATE terminal_billing SET since = ?", usageTimestamp(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTerminalUsage(old); err != nil {
		t.Fatal(err)
	}
	if got := ledgerRefs(t, s, "alice"); len(got) != 0 {
		t.Fatalf("pre-billing request charged: %v", got)
	}
	if err := s.UpsertTerminalUsage(terminalUsage("alice", "req_new", 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTerminalUsage(terminalUsage("alice", "req_new", 90, 900)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTerminalUsage(terminalUsage("alice", "req_new", 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.GetQuota("alice"); q.BalanceMicroUSD != 1_000_000-1000 {
		t.Fatalf("balance = %d, want one charge of 1000", q.BalanceMicroUSD)
	}
}

// 后台请求入库即扣；遥测重发同一请求不重复扣。
func TestBackgroundUsageChargedOnce(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 1_000_000, "grant:seed", "", "root"); err != nil {
		t.Fatal(err)
	}
	ev := terminalUsage("alice", "bg_1", 50, 400)
	ev.Agent, ev.Model = "codex", "gpt-6-astra"
	for range 2 {
		if _, err := s.InsertBackgroundUsage(ev); err != nil {
			t.Fatal(err)
		}
	}
	got := ledgerRefs(t, s, "alice")
	if len(got) != 1 {
		t.Fatalf("ledger = %v", got)
	}
	for _, e := range s.ListLedger(LedgerFilter{User: "alice", Reason: ReasonSpend}) {
		if e.DeltaMicroUSD != -400 || e.Note != "后台请求 · gpt-6-astra" {
			t.Fatalf("entry = %+v", e)
		}
	}
}

// 升级到 schema 23 时，库里已有的终端与后台行都算已结清，重扫不追扣；对话行不受影响。
func TestTerminalBillingMigrationSettlesHistory(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps := migrations()
	if err := runMigrations(db, steps[:22]); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"terminal", "background", "chat"} {
		if _, err := db.Exec(`INSERT INTO usage_events (ts, user, session_id, kind, req_id, cost_micro_usd)
			VALUES ('2026-10-24T00:00:00.000000000Z', 'alice', 's1', ?, ?, 777)`, kind, "r-"+kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := runMigrations(db, steps); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT kind, settled_micro_usd FROM usage_events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var settled int64
		if err := rows.Scan(&kind, &settled); err != nil {
			t.Fatal(err)
		}
		want := int64(777)
		if kind == "chat" {
			want = 0
		}
		if settled != want {
			t.Errorf("%s settled = %d, want %d", kind, settled, want)
		}
	}
	var since string
	if err := db.QueryRow("SELECT since FROM terminal_billing WHERE id = 1").Scan(&since); err != nil || since <= "2026-10-24T00:00:00" {
		t.Fatalf("since = %q, %v", since, err)
	}
}
