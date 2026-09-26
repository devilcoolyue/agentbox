package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestChatTurnCostsScopeSourcesAndBatches(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := UsageEvent{User: "alice", SessionID: "s1", ThreadID: "thread", TurnID: "shared-id", Kind: UsageKindChat, Agent: "codex", CostMicroUSD: 123, Price: &PriceSnapshot{Version: 1, Source: "table"}}
	rows := []UsageEvent{base}
	other := base
	other.SessionID = "other-session"
	rows = append(rows, other)
	other = base
	other.ThreadID = "other-thread"
	rows = append(rows, other)
	other = base
	other.Kind = UsageKindTitle
	rows = append(rows, other)
	other = base
	other.Kind = UsageKindTerminal
	rows = append(rows, other)
	ids := []string{"shared-id", "shared-id", "missing"}
	for i := 0; i < 501; i++ {
		e := base
		e.TurnID = fmt.Sprintf("batch-%d", i)
		rows = append(rows, e)
		ids = append(ids, e.TurnID)
	}
	if err := s.InsertUsage(rows...); err != nil {
		t.Fatal(err)
	}
	got, err := s.ChatTurnCosts("s1", "thread", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 502 || got["shared-id"].CostMicroUSD != 123 || got["shared-id"].Source != "table" {
		t.Fatalf("scope/dedup failed: count=%d value=%+v", len(got), got["shared-id"])
	}
	for id, cost := range got {
		if cost.CostMicroUSD != 123 {
			t.Fatalf("%s: %+v", id, cost)
		}
	}
	// An unpriced submodel must not look like a complete (or free) quote.
	other = base
	other.Price = &PriceSnapshot{Version: 1, Source: "unpriced"}
	other.CostMicroUSD = 0
	if err := s.InsertUsage(other); err != nil {
		t.Fatal(err)
	}
	got, err = s.ChatTurnCosts("s1", "thread", []string{"shared-id"})
	if err != nil {
		t.Fatal(err)
	}
	if got["shared-id"].Source != "mixed" || !got["shared-id"].Partial || got["shared-id"].CostMicroUSD != 123 {
		t.Fatal(got)
	}
}
