package store

import (
	"agentbox/internal/gitaccess"
	"path/filepath"
	"testing"
)

func TestGitBindingsDefaultsOwnershipAndRemoval(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	c := GitConnection{ID: "alice-git", Owner: "alice", Label: "Private", Provider: "gitlab", BaseURL: "https://git.example.com", AuthType: "pat", Secret: []byte("ciphertext"), Enabled: true}
	if _, err = s.SaveGitConnection(c, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.SetGitDefault("bob", "", c.ID); err == nil {
		t.Fatal("another user selected private default")
	}
	sess := Session{ID: "alice-space", User: "alice"}
	if err = s.PutWithGitDefault(sess, c.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GitDefault("alice", sess.ID); err != nil || got != c.ID {
		t.Fatalf("default: %s %v", got, err)
	}
	b := GitBinding{SessionID: sess.ID, Repo: "project", Remote: "origin", URL: "https://git.example.com/group/repo.git", ConnectionID: c.ID}
	if err = s.SaveGitBinding("bob", b, 0); err == nil {
		t.Fatal("another user bound private workspace")
	}
	if err = s.SaveGitBinding("alice", b, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveGitBinding("alice", b, 0); err != ErrGitConflict {
		t.Fatalf("stale binding: %v", err)
	}
	if err = s.DeleteGitConnection("alice", c.ID, 1); err != ErrGitInUse {
		t.Fatalf("deleted referenced connection: %v", err)
	}
	if err = s.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.GitBindings(sess.ID)
	if err != nil || len(bindings) != 0 {
		t.Fatalf("orphan bindings: %+v %v", bindings, err)
	}
	if def, err := s.GitDefault("alice", sess.ID); err != nil || def != "" {
		t.Fatalf("orphan default: %s %v", def, err)
	}
	if err = s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	cs, err := s.GitConnections("alice")
	if err != nil || len(cs) != 0 {
		t.Fatal("deleted user's credentials remain accessible")
	}
}

func TestGitConnectionLateLoginCannotPublishAfterUserRecreation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateToken("old-login", "alice"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "different-user"}); err != nil {
		t.Fatal(err)
	}
	c := GitConnection{ID: "late-oauth", Owner: "alice", Secret: []byte("ciphertext")}
	if _, err = s.SaveGitConnectionWithLogin(c, 0, "old-login"); err == nil {
		t.Fatal("old user's credential published into recreated username")
	}
	rows, err := s.GitConnections("alice")
	if err != nil || len(rows) != 0 {
		t.Fatal("credential leaked")
	}
}

func TestGitConnectionNetworkCannotBeSilentlyChanged(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	c := GitConnection{ID: "private", Owner: "alice", Secret: []byte("ciphertext"), Network: gitaccess.NetworkPolicy{Route: "tunnel"}}
	if _, err = s.SaveGitConnection(c, 0); err != nil {
		t.Fatal(err)
	}
	c.Network.Route = "direct"
	if _, err = s.SaveGitConnection(c, 1); err != ErrGitConflict {
		t.Fatalf("network changed silently: %v", err)
	}
	original, err := s.GitConnection("alice", c.ID)
	if err != nil || original.Network.Route != "tunnel" {
		t.Fatal("immutable network policy lost")
	}
	if string(original.AssociatedData()) == string(c.AssociatedData()) {
		t.Fatal("network not authenticated with ciphertext")
	}
}
