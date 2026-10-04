package syncclient

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"agentbox/internal/syncproto"
)

func historyFixture(t *testing.T, s *StateStore, saved SavedBinding, files int) Batch {
	t.Helper()
	local, remote := map[string]string{}, map[string]string{}
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("file-%04d", i)
		local[name] = "new"
		remote[name] = "old"
	}
	plan, err := BuildPlan(saved.Binding, nil, tree(local), tree(remote), PlanOptions{Direction: PreferLocal})
	if err != nil {
		t.Fatal(err)
	}
	batch := Batch{ID: stateID(), Plan: plan}
	for range plan.Operations {
		batch.Operations = append(batch.Operations, SavedOperation{ID: stateID(), Status: "verified"})
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO batches VALUES (?,?,?)", saved.ID, batch.ID, raw); err != nil {
		t.Fatal(err)
	}
	return batch
}

func TestHistoryPagesBoundFilesWithoutLoss(t *testing.T) {
	s, saved, _ := stateFixture(t)
	batch := historyFixture(t, s, saved, 123)
	cursor := ""
	seen := map[string]bool{}
	for pageNumber, want := range []int{50, 50, 23} {
		page, err := s.historyPage(t.Context(), saved, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.History) != 1 || len(page.History[0].Files) != want || page.History[0].ID != batch.ID || page.History[0].TotalFiles != 123 || page.History[0].FileOffset != pageNumber*50 {
			t.Fatalf("bad page: %+v", page)
		}
		for _, file := range page.History[0].Files {
			if seen[file.ID] {
				t.Fatal("duplicate", file.ID)
			}
			seen[file.ID] = true
		}
		raw, _ := json.Marshal(event{History: page.History, Page: &page})
		if len(raw) >= maxCommand {
			t.Fatal("page exceeds IPC bound")
		}
		if page.Metadata.HistoryBatches != 1 || page.Metadata.Bytes == 0 || page.Metadata.Bytes != page.Metadata.BindingBytes {
			t.Fatal("incorrect logical capacity", page.Metadata)
		}
		cursor = page.NextCursor
	}
	if cursor != "" || len(seen) != 123 {
		t.Fatal("incomplete page traversal")
	}
}

func TestHistoryPagesIncludeEmptyBatchesAndPendingFirst(t *testing.T) {
	s, saved, _ := stateFixture(t)
	for range 25 {
		historyFixture(t, s, saved, 0)
	}
	saved = beginFixture(t, s, saved)
	first, err := s.historyPage(t.Context(), saved, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.History) != 20 || !first.Pending || !first.History[0].Pending || first.History[0].ID != saved.Pending.ID || first.TotalBatches != 26 || first.NextCursor == "" {
		t.Fatal("pending/empty batches lost", first)
	}
	next, err := s.historyPage(t.Context(), saved, first.NextCursor)
	if err != nil || len(next.History) != 6 || next.NextCursor != "" || !next.Pending {
		t.Fatal("bad remaining page", next, err)
	}
	newState, err := s.StartOperation(saved.ID, saved.Revision, saved.Pending.Operations[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.historyPage(t.Context(), newState, first.NextCursor); !errors.Is(err, ErrStateChanged) {
		t.Fatal("stale cursor accepted", err)
	}
	if _, err = s.historyPage(t.Context(), saved, ""); !errors.Is(err, ErrStateChanged) {
		t.Fatal("stale loaded binding accepted", err)
	}
}

func TestHistoryCursorRejectsForeignAndInvalidPosition(t *testing.T) {
	s, saved, _ := stateFixture(t)
	historyFixture(t, s, saved, 2)
	for _, tc := range []struct {
		position historyCursor
		want     error
	}{
		{historyCursor{stateID(), saved.Revision, 0, 0}, ErrStateChanged},
		{historyCursor{saved.ID, saved.Revision, -1, 0}, syncproto.ErrInvalid},
		{historyCursor{saved.ID, saved.Revision, 0, 3}, syncproto.ErrInvalid},
		{historyCursor{saved.ID, saved.Revision, 99, 0}, syncproto.ErrInvalid},
	} {
		raw, _ := json.Marshal(tc.position)
		_, err := s.historyPage(t.Context(), saved, base64.RawURLEncoding.EncodeToString(raw))
		if !errors.Is(err, tc.want) {
			t.Fatalf("%+v: %v", tc.position, err)
		}
	}
}
