package store

import (
	"math"
	"path/filepath"
	"testing"
)

func TestMessageSettlementRestartAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Grant("alice", 1000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	e := UsageEvent{User: "alice", SessionID: "s1", ThreadID: "thread", TurnID: "turn", Agent: "claude", Kind: UsageKindChat, Model: "model", ReqID: "msg-one", InputTokens: 10, CostMicroUSD: 100, Raw: `{"id":"msg-one","usage":{"input_tokens":10}}`, Price: &PriceSnapshot{Version: 1, Source: "table", PerRequest: true, Standard: TokenRates{Input: 10}}}
	bad := e
	bad.ReqID = "msg-two"
	bad.Price = &PriceSnapshot{Standard: TokenRates{Input: math.NaN()}}
	if _, err = s.InsertUsageMessages(e, bad); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	var count int
	if err = s.db.QueryRow("select count(*) from usage_messages").Scan(&count); err != nil || count != 0 {
		t.Fatal("identity claim did not roll back", count, err)
	}
	q, _ := s.GetQuota("alice")
	if q.BalanceMicroUSD != 1000 {
		t.Fatal("partial charge", q)
	}
	if n, err := s.InsertUsageMessages(e, e); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e.TurnID = "resumed"
	if n, err := s.InsertUsageMessages(e); err != nil || n != 0 {
		t.Fatal("replayed after restart", n, err)
	}
	q, _ = s.GetQuota("alice")
	if q.BalanceMicroUSD != 900 {
		t.Fatal(q)
	}
	rows := s.ListUsage(UsageFilter{User: "alice"})
	if len(rows) != 1 || rows[0].InputTokens != 10 {
		t.Fatal(rows)
	}
	// Identity is scoped to a workspace, so another workspace is independent.
	e.SessionID = "s2"
	if n, err := s.InsertUsageMessages(e); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

// A message settled mid-turn gets the turn's latency, the provider name and a
// late request id (claiming telemetry timing parked under it) at the end of
// the turn; its tokens, cost and the ledger stay as settled.
func TestFinishUsageRowsAfterLiveSettlement(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Grant("alice", 1000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	e := UsageEvent{User: "alice", SessionID: "s1", ThreadID: "thread", TurnID: "turn", Agent: "claude", Kind: UsageKindChat, Model: "model", ReqID: "msg-one", InputTokens: 10, CostMicroUSD: 100, Raw: `{"id":"msg-one"}`, Price: &PriceSnapshot{Version: 1, Source: "table", Standard: TokenRates{Input: 10}}}
	if n, err := s.InsertUsageMessages(e); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err = s.RecordRequestTimings([]RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_one", TTFTMs: 800, DurationMS: 4000}}); err != nil {
		t.Fatal(err)
	}
	done := e
	done.ReqID, done.CostMicroUSD, done.InputTokens = "", 999, 999 // ignored
	done.TTFTMs, done.DurationMS, done.WallMS, done.Provider, done.APIRequestID = 700, 2500, 3000, "firstParty", "req_one"
	if err = s.FinishUsageRows(done); err != nil {
		t.Fatal(err)
	}
	rows := s.ListUsage(UsageFilter{User: "alice"})
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	r := rows[0]
	if r.TTFTMs != 700 || r.DurationMS != 2500 || r.WallMS != 3000 || r.Provider != "firstParty" ||
		r.APIRequestID != "req_one" || r.APITTFTMs != 800 || r.APIDurationMS != 4000 || r.InputTokens != 10 || r.CostMicroUSD != 100 {
		t.Fatalf("%+v", r)
	}
	if q, _ := s.GetQuota("alice"); q.BalanceMicroUSD != 900 {
		t.Fatal(q)
	}
	// An empty provider keeps the one on record.
	done.Provider = ""
	if err = s.FinishUsageRows(done); err != nil {
		t.Fatal(err)
	}
	if r := s.ListUsage(UsageFilter{User: "alice"})[0]; r.Provider != "firstParty" {
		t.Fatalf("%+v", r)
	}
	other := done
	other.TurnID = "another"
	if err = s.FinishUsageRows(other); err == nil {
		t.Fatal("finished a row the turn never wrote")
	}
}
