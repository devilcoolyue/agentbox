package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// 一个回合的用量行，费用可调。
func chatUsage(user string, cost int64) UsageEvent {
	return UsageEvent{
		User: user, SessionID: "s1", ThreadID: "t1", TurnID: "turn1",
		Agent: "claude", Model: "claude-opus-4-8",
		InputTokens: 100, OutputTokens: 200, CostMicroUSD: cost,
	}
}

// 没开额度的用户照常记用量，但一分钱都不该动——老部署升级上来全是这种用户，
// 这条错了就是所有人被拦在门外。
func TestUsageUnmeteredUserUntouched(t *testing.T) {
	s := newStore(t)
	if err := s.InsertUsage(chatUsage("alice", 103081)); err != nil {
		t.Fatal(err)
	}
	if got := len(s.ListUsage(UsageFilter{User: "alice"})); got != 1 {
		t.Fatalf("用量应照记，得到 %d 行", got)
	}
	if got := len(s.ListLedger(LedgerFilter{User: "alice"})); got != 0 {
		t.Errorf("未开额度的用户不该产生账本流水，得到 %d 条", got)
	}
	if _, ok := s.GetQuota("alice"); ok {
		t.Error("不该被动给用户建出额度行")
	}
}

// 开了额度就要真扣，且扣的钱要能在账本里对上。
func TestUsageChargesBalance(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 1_000_000, "grant:seed", "首充 $1", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsage(
		chatUsage("alice", 621),
		chatUsage("alice", 103081),
	); err != nil {
		t.Fatal(err)
	}
	q, ok := s.GetQuota("alice")
	if !ok {
		t.Fatal("额度行不见了")
	}
	if want := int64(1_000_000 - 621 - 103081); q.BalanceMicroUSD != want {
		t.Errorf("余额 = %d，想要 %d", q.BalanceMicroUSD, want)
	}
	if q.GrantedMicroUSD != 1_000_000 {
		t.Errorf("累计充值 = %d，想要 1000000", q.GrantedMicroUSD)
	}
	if q.SpentMicroUSD != 621+103081 {
		t.Errorf("累计消耗 = %d，想要 %d", q.SpentMicroUSD, 621+103081)
	}
	// 一充两扣，三条流水；同回合按模型拆出的两行各自入账。
	if got := len(s.ListLedger(LedgerFilter{User: "alice"})); got != 3 {
		t.Errorf("账本条数 = %d，想要 3", got)
	}
	cached, ledger, err := s.RecomputeBalance("alice")
	if err != nil {
		t.Fatal(err)
	}
	if cached != ledger {
		t.Errorf("缓存余额 %d 与账本重算 %d 不一致", cached, ledger)
	}
}

// 幂等键：同一个 ref 重放不得再扣一次钱。这是重试与补账安全的全部依据。
func TestGrantIdempotent(t *testing.T) {
	s := newStore(t)
	q1, err := s.Grant("alice", 500_000, "grant:order-42", "订单 42", "boxadmin")
	if err != nil {
		t.Fatal(err)
	}
	q2, err := s.Grant("alice", 500_000, "grant:order-42", "订单 42", "boxadmin")
	if err != ErrDuplicateRef {
		t.Fatalf("重复 ref 应返回 ErrDuplicateRef，得到 %v", err)
	}
	if q1.BalanceMicroUSD != 500_000 || q2.BalanceMicroUSD != 500_000 {
		t.Errorf("重放把余额充成了两份: %d / %d", q1.BalanceMicroUSD, q2.BalanceMicroUSD)
	}
	if got := len(s.ListLedger(LedgerFilter{User: "alice"})); got != 1 {
		t.Errorf("账本条数 = %d，想要 1", got)
	}
}

// 每条用量行的扣款以它自己的行 id 为幂等键，将来给历史行补价重算时不会重复扣。
func TestUsageChargeRefIsRowID(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 10_000, "grant:seed", "", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsage(chatUsage("alice", 621)); err != nil {
		t.Fatal(err)
	}
	rows := s.ListUsage(UsageFilter{User: "alice"})
	if len(rows) != 1 {
		t.Fatalf("用量 %d 行", len(rows))
	}
	wantRef := fmt.Sprintf("usage:%d", rows[0].ID)
	entries := s.ListLedger(LedgerFilter{User: "alice", Reason: ReasonSpend})
	if len(entries) != 1 || entries[0].Ref != wantRef {
		t.Fatalf("扣款流水的 ref = %+v，想要 %s", entries, wantRef)
	}
	if entries[0].DeltaMicroUSD != -621 {
		t.Errorf("扣款金额 = %d，想要 -621", entries[0].DeltaMicroUSD)
	}
}

// 余额只够一半的回合允许超支成负数：花掉的钱是真花掉了，账要认。
// 下一回合由服务端按余额 <= 0 拦住（见 server.quotaBlock）。
func TestBalanceGoesNegativeOnOvershoot(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 1000, "grant:seed", "", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsage(chatUsage("alice", 103081)); err != nil {
		t.Fatal(err)
	}
	q, _ := s.GetQuota("alice")
	if q.BalanceMicroUSD != 1000-103081 {
		t.Errorf("余额 = %d，想要 %d（超支必须记成负数，不能截到 0）", q.BalanceMicroUSD, 1000-103081)
	}
}

// 费用为 0 的行（codex 没配价目表）只记不扣，也不该产生空流水。
func TestZeroCostDoesNotCharge(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 1000, "grant:seed", "", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsage(chatUsage("alice", 0)); err != nil {
		t.Fatal(err)
	}
	q, _ := s.GetQuota("alice")
	if q.BalanceMicroUSD != 1000 {
		t.Errorf("余额被动了: %d", q.BalanceMicroUSD)
	}
	if got := len(s.ListLedger(LedgerFilter{User: "alice", Reason: ReasonSpend})); got != 0 {
		t.Errorf("不该有扣款流水，得到 %d 条", got)
	}
}

func TestSetQuotaEnforcedAndRemove(t *testing.T) {
	s := newStore(t)
	q, err := s.SetQuotaEnforced("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if q.Enforced || q.BalanceMicroUSD != 0 {
		t.Fatalf("新开的额度行应是 enforced=false 余额 0: %+v", q)
	}
	// 只计不拦期间照样扣钱，只是不阻止使用。
	if err := s.InsertUsage(chatUsage("alice", 621)); err != nil {
		t.Fatal(err)
	}
	if q, _ = s.GetQuota("alice"); q.BalanceMicroUSD != -621 {
		t.Errorf("余额 = %d，想要 -621", q.BalanceMicroUSD)
	}
	if q, err = s.SetQuotaEnforced("alice", true); err != nil || !q.Enforced {
		t.Fatalf("开启拦截失败: %+v %v", q, err)
	}
	// 余额不该被开关动过。
	if q.BalanceMicroUSD != -621 {
		t.Errorf("切换 enforced 改动了余额: %d", q.BalanceMicroUSD)
	}

	if err := s.RemoveQuota("alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetQuota("alice"); ok {
		t.Error("解除限额后不该还有额度行")
	}
	// 账本是历史，解除限额不抹掉它。
	if got := len(s.ListLedger(LedgerFilter{User: "alice"})); got == 0 {
		t.Error("账本被一起删了，历史消耗查不回来")
	}
}

// 删号必须带走额度行：否则同名新用户会继承前任的余额（或欠款）。
func TestDeleteUserDropsQuota(t *testing.T) {
	s := newStore(t)
	if err := s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant("alice", 1_000_000, "grant:seed", "", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if q, ok := s.GetQuota("alice"); ok {
		t.Errorf("额度行仍在，同名新用户会白捡 %d 微美元", q.BalanceMicroUSD)
	}
}

func TestGrantRejectsBadInput(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 0, "grant:x", "", "boxadmin"); err == nil {
		t.Error("金额为 0 应报错")
	}
	if _, err := s.Grant("alice", 100, "", "", "boxadmin"); err == nil {
		t.Error("缺少幂等键应报错")
	}
}

// 负数充值是管理员冲正，reason 要能和真实消耗区分开，否则报表里「花了多少」
// 会把纠错也算成消耗。
func TestNegativeGrantIsAdjust(t *testing.T) {
	s := newStore(t)
	if _, err := s.Grant("alice", 1_000_000, "grant:seed", "", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant("alice", -400_000, "grant:fix", "充多了", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	entries := s.ListLedger(LedgerFilter{User: "alice", Reason: ReasonAdjust})
	if len(entries) != 1 || entries[0].DeltaMicroUSD != -400_000 {
		t.Fatalf("冲正流水不对: %+v", entries)
	}
	q, _ := s.GetQuota("alice")
	if q.BalanceMicroUSD != 600_000 {
		t.Errorf("余额 = %d，想要 600000", q.BalanceMicroUSD)
	}
}
