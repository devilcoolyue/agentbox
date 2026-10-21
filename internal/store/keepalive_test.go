package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkspaceKeepaliveLivesWithItsWorkspace(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.CreateUser(User{Name: "alice", Role: RoleUser}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w1", "w2"} {
		if err = st.Put(Session{ID: id, User: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	if k, err := st.Keepalive("w1"); err != nil || k != (WorkspaceKeepalive{}) {
		t.Fatalf("unset workspace: %+v %v", k, err)
	}
	want := WorkspaceKeepalive{Enabled: true, Terminal: true, MaxHours: 2, MinContextTokens: 50000}
	if err = st.SetKeepalive("w1", want); err != nil {
		t.Fatal(err)
	}
	if err = st.SetKeepalive("w1", want); err != nil { // saving again updates the row
		t.Fatal(err)
	}
	if k, err := st.Keepalive("w1"); err != nil || k != want {
		t.Fatalf("saved %+v %v", k, err)
	}
	if err = st.SetKeepalive("gone", want); !errors.Is(err, ErrKeepaliveWorkspace) {
		t.Fatalf("saved for a missing workspace: %v", err)
	}
	if err = st.SetKeepalive("w2", want); err != nil {
		t.Fatal(err)
	}
	if err = st.Delete("w1"); err != nil {
		t.Fatal(err)
	}
	if err = st.Put(Session{ID: "w1", User: "alice"}); err != nil {
		t.Fatal(err)
	}
	if k, _ := st.Keepalive("w1"); k.Enabled {
		t.Fatal("a recreated workspace inherited the setting")
	}
	if err = st.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err = st.db.QueryRow("SELECT COUNT(*) FROM workspace_keepalive").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows left after the user was deleted: %d %v", rows, err)
	}
}
