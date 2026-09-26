package store

import (
	"path/filepath"
	"testing"
)

func TestGitOperationCursorAndRecovery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var unfinished int64
	for i := 0; i < 6; i++ {
		owner := "alice"
		if i%2 != 0 {
			owner = "bob"
		}
		id, err := s.BeginGitOperation(owner, "s1", "repo", "conn", "fetch", "origin")
		if err != nil {
			t.Fatal(err)
		}
		if i == 4 {
			unfinished = id
		} else if err = s.FinishGitOperation(id, "success"); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.GitOperations("alice", 0, 2)
	if err != nil || len(page) != 2 || page[0].ID != unfinished {
		t.Fatalf("page: %+v %v", page, err)
	}
	next, err := s.GitOperations("alice", page[1].ID, 2)
	if err != nil || len(next) != 1 || next[0].ID >= page[1].ID {
		t.Fatalf("cursor: %+v %v", next, err)
	}
	if err = s.RecoverGitOperations(); err != nil {
		t.Fatal(err)
	}
	page, err = s.GitOperations("alice", 0, 2)
	if err != nil || page[0].Result != "interrupted_unknown" || page[0].FinishedAt == "" || page[1].Result != "success" {
		t.Fatalf("recovery: %+v %v", page, err)
	}
	if _, err = s.GitOperations("alice", 0, 101); err == nil {
		t.Fatal("unbounded query accepted")
	}
}
