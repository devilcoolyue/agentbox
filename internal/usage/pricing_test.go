package usage

import (
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func TestPriceSnapshotsSurviveEditsAndTerminalGrowth(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 2, Output: 3}, LongContextOver: 100, Long: &config.TokenRates{Input: 4, Output: 6}}}
	if _, err := s.store.Grant("alice", 10000, "test-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	base := store.UsageEvent{User: "alice", Agent: "codex", SessionID: "s1", TurnID: "t1", InputTokens: 10, OutputTokens: 10, Kind: store.UsageKindChat}
	priced := s.Price(base)
	if err := s.store.InsertUsage(priced); err != nil {
		t.Fatal(err)
	}
	terminal := base
	terminal.Kind = store.UsageKindTerminal
	terminal.ReqID = "terminal-test"
	terminal.TurnID = "terminal-test"
	if err := s.store.UpsertTerminalUsage(s.Price(terminal)); err != nil {
		t.Fatal(err)
	}
	s.cfg.Pricing["codex"] = config.ModelPrice{TokenRates: config.TokenRates{Input: 100, Output: 100}}
	terminal.InputTokens = 110
	if err := s.store.UpsertTerminalUsage(s.Price(terminal)); err != nil {
		t.Fatal(err)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	if len(rows) != 2 {
		t.Fatal(rows)
	}
	for _, row := range rows {
		if row.Price == nil || row.Price.Standard.Input != 2 {
			t.Fatalf("lost historical snapshot: %+v", row)
		}
		want := int64(50)
		if row.Kind == store.UsageKindTerminal {
			want = 500
		}
		if row.CostMicroUSD != want {
			t.Fatalf("repriced history: got %d want %d", row.CostMicroUSD, want)
		}
	}
	quota, _ := s.store.GetQuota("alice")
	if quota.BalanceMicroUSD != 9950 {
		t.Fatalf("terminal charged: %+v", quota)
	}
}
func TestUnpricedAndProviderSourcesArePersisted(t *testing.T) {
	s, _ := newTestServer(t)
	zero := s.Price(store.UsageEvent{Agent: "codex", InputTokens: 10})
	if zero.Price.Source != "unpriced" || zero.CostMicroUSD != 0 {
		t.Fatal(zero)
	}
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 2}}}
	supplied := s.Price(store.UsageEvent{Agent: "claude", InputTokens: 10, CostMicroUSD: 99})
	fallback := s.Price(store.UsageEvent{Agent: "claude", InputTokens: 10})
	if supplied.Price.Source != "provider" || supplied.CostMicroUSD != 99 || fallback.Price.Source != "table" || fallback.CostMicroUSD != 20 {
		t.Fatal("incorrect source")
	}
}
func TestFlushUsesOneTransactionAndDoesNotDoubleCharge(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 1}}}
	if _, err := s.store.Grant("alice", 100, "test-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	var tally Tally
	tally.Observe(store.UsageEvent{User: "alice", Agent: "codex", TurnID: "t1"}, []byte(`{"type":"turn.completed","usage":{"input_tokens":150,"output_tokens":1}}`))
	if n := s.Flush(&tally, time.Second); n != 1 {
		t.Fatal(n)
	}
	if n := s.Flush(&tally, time.Second); n != 0 {
		t.Fatal(n)
	}
	q, _ := s.store.GetQuota("alice")
	if q.BalanceMicroUSD != -50 {
		t.Fatalf("overdraft invariant changed %+v", q)
	}
}
