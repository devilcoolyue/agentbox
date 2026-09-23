package store

import (
	"math"
	"path/filepath"
	"testing"
)

func TestSnapshotFailureRollsBackUsageAndCredit(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Grant("alice", 1000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	valid := UsageEvent{User: "alice", TurnID: "t1", CostMicroUSD: 100, Price: &PriceSnapshot{Version: 1, Source: "provider"}}
	invalid := valid
	invalid.TurnID = "t2"
	invalid.Price = &PriceSnapshot{Version: 1, Source: "table", Standard: TokenRates{Input: math.NaN()}}
	if err := st.InsertUsage(valid, invalid); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if rows := st.ListUsage(UsageFilter{User: "alice"}); len(rows) != 0 {
		t.Fatal("partial usage insert")
	}
	q, _ := st.GetQuota("alice")
	if q.BalanceMicroUSD != 1000 {
		t.Fatal("partial debit")
	}
}
