package store

import (
	"path/filepath"
	"testing"
)

func TestGitProfilePersistenceAndUserRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetGitProfile("alice")
	if err != nil || p.Name != "alice" || p.Email != "alice@localhost" {
		t.Fatalf("default: %+v %v", p, err)
	}
	if _, err := s.SetGitProfile("alice", "Alice Example", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err = s.GetGitProfile("alice")
	if err != nil || p.Name != "Alice Example" || p.UpdatedAt.IsZero() {
		t.Fatalf("persisted: %+v %v", p, err)
	}
	bob, err := s.GetGitProfile("bob")
	if err != nil || bob.Email != "bob@localhost" {
		t.Fatalf("isolation: %+v %v", bob, err)
	}
	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	p, err = s.GetGitProfile("alice")
	if err != nil || p.Name != "alice" {
		t.Fatalf("deleted: %+v %v", p, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGitProfile("alice"); err == nil {
		t.Fatal("database failure silently returned a default identity")
	}
}
