package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestUsageReasoningMigrationAndTerminalBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db, migrations()[:16]); err != nil {
		t.Fatal(err)
	}
	// Two pre-upgrade rows: one with a locked price and one predating price
	// snapshots. Both must keep their original cost when only effort is added.
	for _, id := range []string{"snapshot", "legacy"} {
		price := ""
		if id == "snapshot" {
			price = `{"version":1,"source":"table","key":"codex","standard":{"input":2}}`
		}
		if _, err := db.Exec(`INSERT INTO usage_events
			(ts,user,session_id,turn_id,agent,kind,req_id,input_tokens,cost_micro_usd,price_snapshot)
			VALUES ('2026-09-23T00:00:00.000000000Z','alice','space',?,'codex','terminal',?,10,20,?)`, id, id, price); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Grant("alice", 1000, "fixture-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	rows := s.ListUsage(UsageFilter{})
	if len(rows) != 2 {
		t.Fatalf("migration lost rows: %+v", rows)
	}
	for _, row := range rows {
		if row.Effort != "" || row.EffortControl != "" || row.CostMicroUSD != 20 {
			t.Fatalf("migration invented historical values: %+v", row)
		}
		row.Effort, row.EffortControl = "high", "effort"
		row.Price = &PriceSnapshot{Version: 1, Source: "table", Standard: TokenRates{Input: 99}}
		row.CostMicroUSD = 990
		for range 2 {
			if err := s.UpsertTerminalUsage(row); err != nil {
				t.Fatal(err)
			}
		}
	}
	backfilled := s.ListUsage(UsageFilter{})
	if len(backfilled) != 2 {
		t.Fatalf("backfill duplicated rows: %+v", backfilled)
	}
	for _, row := range backfilled {
		if row.Effort != "high" || row.EffortControl != "effort" || row.CostMicroUSD != 20 || row.InputTokens != 10 {
			t.Fatalf("backfill lost effort or changed accounting: %+v", row)
		}
		if row.ReqID == "snapshot" && (row.Price == nil || row.Price.Standard.Input != 2) {
			t.Fatalf("backfill changed locked price: %+v", row)
		}
		if row.ReqID == "legacy" && row.Price != nil {
			t.Fatalf("backfill invented historical price: %+v", row)
		}
	}
	q, _ := s.GetQuota("alice")
	if q.BalanceMicroUSD != 1000 {
		t.Fatal("terminal reasoning backfill charged balance")
	}
	// A later scan discovers mixed efforts in a growing turn. Clear the label
	// instead of retaining the first observed effort as if it covered all usage.
	row := backfilled[0]
	row.Effort, row.EffortControl = "", ""
	if err := s.UpsertTerminalUsage(row); err != nil {
		t.Fatal(err)
	}
	for _, got := range s.ListUsage(UsageFilter{}) {
		if got.ID == row.ID && (got.Effort != "" || got.EffortControl != "") {
			t.Fatalf("stale effort survived rescan: %+v", got)
		}
	}
}
