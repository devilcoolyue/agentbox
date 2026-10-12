package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
)

func TestCollaborationParticipantsCannotUseOrdinarySessionRoutes(t *testing.T) {
	s, source, handler := clientTestServer(t)
	u, ok := s.store.GetUser(source.User)
	if !ok {
		t.Fatal("missing fixture user")
	}
	roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "codex", AccountID: "main", Model: "planner"}, Executor: store.CollaborationBinding{AgentType: "claude", AccountID: "worker", Model: "builder"}}
	cfg := store.CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	input := store.CollaborationCreateInput{Scope: "test", SourceSessionID: source.ID, Repository: ".", Baseline: strings.Repeat("a", 40), Text: "Synthetic work", Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	r, _, err := s.store.CreateCollaboration(u, strings.Repeat("b", 32), input, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.store.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	main := store.Session{ID: "internal-planner", User: u.Name, Agent: "codex", AccountID: "main", DefaultModel: "planner", Status: store.StatusStopped, CreatedAt: time.Now()}
	worker := store.Session{ID: "internal-worker", User: u.Name, Agent: "claude", AccountID: "worker", DefaultModel: "builder", Status: store.StatusStopped, CreatedAt: time.Now()}
	if _, _, err = s.store.PublishCollaborationParticipants(u, r.ID, r.Revision, main, worker, store.CollaborationStepInput{Text: "plan"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{main.ID, worker.ID} {
		for _, route := range [][2]string{{"POST", "/start"}, {"POST", "/stop"}, {"DELETE", ""}, {"PUT", "/account"}, {"GET", "/chat"}, {"PUT", "/file?path=test.txt"}, {"POST", "/client-terminals"}, {"GET", "/files"}, {"GET", "/history"}} {
			w := clientRequest(handler, u.Name, route[0], "/api/sessions/"+id+route[1], `{}`)
			if w.Code != 404 {
				t.Fatalf("managed session reachable through %v: %d %s", route, w.Code, w.Body.String())
			}
		}
	}
	w := clientRequest(handler, u.Name, "GET", "/api/sessions", "")
	var sessions []store.Session
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &sessions) != nil || len(sessions) != 1 || sessions[0].ID != source.ID {
		t.Fatal("public listing leaked participants", w.Code, w.Body.String())
	}
	if len(s.store.All()) != 3 {
		t.Fatal("managed sessions missing from lifecycle/usage enumeration")
	}
}
