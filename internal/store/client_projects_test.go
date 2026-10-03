package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestClientMetadataMigrationPreservesVersionNineSessions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(db, migrations()[:9]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO sessions(id,user,name,agent,account_id,default_model,container_id,status,chat_session,created_at,updated_at) VALUES ('legacy','alice','Keep','claude','shared','old-model','container','running','provider','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	old, ok := st.Get("legacy")
	if !ok || old.AccountID != "shared" || old.DefaultModel != "old-model" || old.ChatSession != "provider" || old.ContainerID != "container" {
		t.Fatalf("old semantics changed: %+v", old)
	}
	projects, err := st.ClientProjects(old.ID)
	if err != nil || len(projects) != 0 {
		t.Fatalf("migration invented projects: %+v %v", projects, err)
	}
	p, err := st.CreateClientProject(ClientProject{SessionID: old.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := st.CreateClientTerminal(old.ID, p.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.BeginCloseClientTerminal(old.ID, terminal.ID); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.ClientTerminal(old.ID, terminal.ID)
	if err != nil || got.State != "closing" {
		t.Fatalf("restart forgot pending teardown: %+v %v", got, err)
	}
	if current, _ := st.Get(old.ID); current != old {
		t.Fatalf("metadata altered session %+v", current)
	}
	if err = st.Delete(old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClientProject(old.ID, p.ID); !errors.Is(err, ErrClientMissing) {
		t.Fatal("orphan project", err)
	}
	if _, err = st.ClientTerminal(old.ID, terminal.ID); !errors.Is(err, ErrClientMissing) {
		t.Fatal("orphan terminal", err)
	}
	if _, err = st.CreateClientProject(ClientProject{SessionID: old.ID, Name: "late", Path: "."}); !errors.Is(err, ErrClientMissing) {
		t.Fatal("recreated deleted workspace", err)
	}
}

func TestClientProjectsRevisionIsolationAndTerminalSnapshots(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"alice", "bob"} {
		if err = st.Put(Session{ID: id, User: id}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := st.CreateClientProject(ClientProject{SessionID: "alice", Name: "P", Path: "project", Arguments: []string{"--model", "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	if !ValidClientResourceID(p.ID) {
		t.Fatal("unstable ID format")
	}
	if _, err = st.ClientProject("bob", p.ID); !errors.Is(err, ErrClientMissing) {
		t.Fatal("cross-session project", err)
	}
	if _, err = st.CreateClientTerminal("bob", p.ID, "agent"); !errors.Is(err, ErrClientMissing) {
		t.Fatal("cross-session terminal", err)
	}
	terminal, err := st.CreateClientTerminal("alice", p.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}
	old := p
	p.Name = "Renamed"
	p.Arguments = []string{"--model", "new"}
	p, err = st.UpdateClientProject(p)
	if err != nil || p.ID != old.ID || p.Revision != 2 {
		t.Fatalf("rename: %+v %v", p, err)
	}
	if _, err = st.UpdateClientProject(old); !errors.Is(err, ErrClientConflict) {
		t.Fatal("stale write", err)
	}
	got, err := st.ClientTerminal("alice", terminal.ID)
	if err != nil || got.Arguments[1] != "fixture" {
		t.Fatalf("modified existing terminal startup: %+v %v", got, err)
	}
	p.Path = "other"
	if _, err = st.UpdateClientProject(p); !errors.Is(err, ErrClientConflict) {
		t.Fatal("moved active project", err)
	}
	if err = st.DeleteClientProject("alice", p.ID, p.Revision); !errors.Is(err, ErrClientConflict) {
		t.Fatal("deleted live project", err)
	}
	if err = st.BeginCloseClientTerminal("alice", terminal.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteClientTerminal("alice", terminal.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.UpdateClientProject(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteClientProject("alice", p.ID, p.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestClientProjectPathRejectsAmbiguousOrEscapingPaths(t *testing.T) {
	for _, value := range []string{"", "..", "../x", "/workspace", "a/../b", "./x", "a//b", "a/", "a\\b", "a\x00b", "a\nb"} {
		if ClientProjectPath(value) {
			t.Errorf("accepted %q", value)
		}
	}
	for _, value := range []string{".", "中文 项目", "a/b", "a'b"} {
		if !ClientProjectPath(value) {
			t.Errorf("rejected %q", value)
		}
	}
}
