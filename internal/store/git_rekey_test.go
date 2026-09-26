package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func TestGitRekeyTransactionRollback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err = s.SaveGitConnection(GitConnection{ID: id, Owner: "alice", Label: id, Provider: "gitlab", BaseURL: "https://git.example.com", AuthType: "pat", Enabled: true, Secret: []byte("old-" + id)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("interrupted")
	if _, err = s.RekeyGitCredentials(func(c GitConnection) ([]byte, error) {
		if c.ID == "b" {
			return nil, failure
		}
		return []byte("new"), nil
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		c, err := s.GitConnection("alice", id)
		if err != nil || !bytes.Equal(c.Secret, []byte("old-"+id)) || c.Revision != 1 {
			t.Fatal("partial rekey committed", err)
		}
	}
	n, err := s.RekeyGitCredentials(func(c GitConnection) ([]byte, error) { return []byte("new-" + c.ID), nil })
	if err != nil || n != 2 {
		t.Fatal("rekey failed", err)
	}
}
