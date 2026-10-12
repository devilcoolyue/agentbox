package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollaborationMigrationFromSchema12PreservesChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(db, migrations()[:12]); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db}
	if err = old.CreateUser(User{Name: "alice", Role: RoleUser, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	u, _ := old.GetUser("alice")
	if err = old.Put(Session{ID: "existing", User: u.Name, Agent: "codex", DefaultModel: "keep-model"}); err != nil {
		t.Fatal(err)
	}
	input := ChatRequestInput{Scope: "chat-scope", ThreadID: "thread", Text: "keep existing conversation", Model: "keep-model"}
	raw, fp, err := encodeChatRequest(input)
	if err != nil {
		t.Fatal(err)
	}
	now := usageTimestamp(time.Now())
	_, err = old.db.Exec("INSERT INTO chat_requests(user,user_created_at,session_id,request_id,thread_id,turn_id,fingerprint,request_json,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", u.Name, ownerEpoch(u), "existing", "request", "thread", "old-turn", fp, string(raw), ChatAccepted, now, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := old.ChatRequest(u, "existing", "request")
	if err != nil {
		t.Fatal(err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	after, err := st.ChatRequest(u, "existing", "request")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("migration rewrote chat receipt")
	}
	sess, ok := st.Get("existing")
	if !ok || sess.DefaultModel != "keep-model" {
		t.Fatal("migration lost existing session")
	}
	var version int
	if err = st.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
	if _, err = st.ListCollaborations(u); err != nil {
		t.Fatal(err)
	}
}

func TestCollaborationScopeRebindingIsAuditedAndIdempotent(t *testing.T) {
	s, u, r := preparedCollaboration(t, true)
	if err := s.BindCollaborationScope(u, "new-scope"); err == nil {
		t.Fatal("scope changed under runnable task")
	}
	if err := s.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	if err := s.BindCollaborationScope(u, "new-scope"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Collaboration(u, r.ID)
	if err != nil || r.Scope != "new-scope" || r.Request.Scope == r.Scope {
		t.Fatal(r, err)
	}
	if err = s.BindCollaborationScope(u, "new-scope"); err != nil {
		t.Fatal(err)
	}
	again, err := s.Collaboration(u, r.ID)
	if err != nil || again.Revision != r.Revision {
		t.Fatal("startup rebinding not idempotent", again, err)
	}
	events, err := s.CollaborationEvents(u, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range events {
		if e.Type == "scope_changed" {
			found++
		}
	}
	if found != 1 {
		t.Fatal("missing or duplicate scope audit", found)
	}
}

func TestCollaborationMigrationFromSchema13PreservesUnboundTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = runMigrations(db, migrations()[:13]); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db}
	u := User{Name: "alice", Role: RoleUser, CreatedAt: time.Now()}
	if err = old.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	u, _ = old.GetUser(u.Name)
	if err = old.Put(Session{ID: "source", User: u.Name, Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	roles := CollaborationRoles{Coordinator: CollaborationBinding{AgentType: "claude", AccountID: "a", Model: "m"}, Executor: CollaborationBinding{AgentType: "codex", AccountID: "b", Model: "m"}}
	in := CollaborationCreateInput{Scope: "scope", SourceSessionID: "source", Text: "keep original", Baseline: strings.Repeat("a", 40), Repository: ".", Roles: roles, MaxSteps: 20}
	cfg := CollaborationConfig{Roles: roles, MaxSteps: 20}
	before, _, err := old.CreateCollaboration(u, strings.Repeat("b", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	after, err := upgraded.Collaboration(u, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("schema14 rewrote original task")
	}
	rows, err := upgraded.CollaborationThreadRuns(u, "source", "thread")
	if err != nil || len(rows) != 0 {
		t.Fatal("old task assigned a guessed thread", rows, err)
	}
	if _, fresh, err := upgraded.CreateCollaboration(u, strings.Repeat("b", 32), in, cfg); err != nil || fresh {
		t.Fatal("migration broke replay", err)
	}
}
