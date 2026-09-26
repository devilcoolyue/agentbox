package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestGitSharedConnectionPermissionsAndRevocation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, u := range []User{{Name: "admin", Role: RoleAdmin}, {Name: "alice", Role: RoleUser}, {Name: "bob", Role: RoleUser}, {Name: "otheradmin", Role: RoleAdmin}} {
		if err = s.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	c := GitConnection{ID: "bot", Owner: "admin", Label: "Robot", AuthType: "pat", Enabled: true, Secret: []byte("ciphertext")}
	if _, err = s.SaveGitConnection(c, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GitConnectionFor("otheradmin", c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("admin implicitly used someone else's private connection")
	}
	if err = s.SetGitShares("admin", c.ID, 1, []GitShare{{User: "alice"}, {User: "bob", Write: true}}); err != nil {
		t.Fatal(err)
	}
	alice, err := s.GitConnectionFor("alice", c.ID)
	if err != nil || !alice.ReadOnly || alice.Managed || alice.Actor != "alice" {
		t.Fatalf("read-only grant: %+v %v", alice, err)
	}
	bob, err := s.GitConnectionFor("bob", c.ID)
	if err != nil || bob.ReadOnly {
		t.Fatalf("write grant: %+v %v", bob, err)
	}
	rows, err := s.GitConnectionsFor("alice")
	if err != nil || len(rows) != 1 || len(rows[0].Secret) != 0 {
		t.Fatal("list permission/redaction failed")
	}
	if err = s.SetGitShares("alice", c.ID, 2, nil); err == nil {
		t.Fatal("recipient changed ACL")
	}
	if err = s.PutWithGitDefault(Session{ID: "alice-space", User: "alice"}, c.ID); err != nil {
		t.Fatal(err)
	}
	binding := GitBinding{SessionID: "alice-space", Remote: "origin", URL: "https://git.example/repo", ConnectionID: c.ID}
	if err = s.SaveGitBinding("alice", binding, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.SetGitShares("admin", c.ID, 2, []GitShare{{User: "bob", Write: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GitConnectionFor("alice", c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("revoked connection usable")
	}
	if def, err := s.GitDefault("alice", "alice-space"); err != nil || def != "" {
		t.Fatal("revoked default remains")
	}
	if err = s.SetGitDefault("alice", "", c.ID); err == nil {
		t.Fatal("revoked account selected as default")
	}
	if err = s.DeleteUser("bob"); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser(User{Name: "bob", Role: RoleUser}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GitConnectionFor("bob", c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("recreated username inherited access")
	}
	if err = s.DeleteUser("admin"); err != nil {
		t.Fatal(err)
	}
	rows, err = s.GitConnectionsFor("alice")
	if err != nil || len(rows) != 0 {
		t.Fatal("deleted owner connection survives")
	}
}
