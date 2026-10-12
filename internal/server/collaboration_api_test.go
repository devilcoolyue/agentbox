package server

import (
	"encoding/json"
	"strings"
	"testing"

	"agentbox/internal/store"
)

func TestCollaborationAPIGateOwnershipAndPendingLookup(t *testing.T) {
	s, source, h := clientTestServer(t)
	u, _ := s.store.GetUser(source.User)
	if w := clientRequest(h, u.Name, "GET", "/api/collaboration/runs", ""); w.Code != 404 {
		t.Fatal("disabled feature accessible", w.Code)
	}
	if err := s.cfg.SetCollaborationEnabled(true); err != nil {
		t.Fatal(err)
	}
	if w := clientRequest(h, "", "GET", "/api/collaboration/options", ""); w.Code != 401 {
		t.Fatal("unauthenticated options", w.Code)
	}
	w := clientRequest(h, u.Name, "GET", "/api/collaboration/options", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "credentials_dir") || strings.Contains(w.Body.String(), "base_url") || strings.Contains(w.Body.String(), "env") {
		t.Fatal("options leaked account secret metadata")
	}
	roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}, Executor: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}}
	input := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: source.ID, Repository: ".", Baseline: strings.Repeat("a", 40), Text: "fixture task", Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	cfg := store.CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	request := strings.Repeat("b", 32)
	r, _, err := s.store.CreateCollaboration(u, request, input, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{r.ID, request} {
		w = clientRequest(h, u.Name, "GET", "/api/collaboration/runs/"+id, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "fixture task") {
			t.Fatal("creation lookup failed", w.Code, w.Body.String())
		}
		var reply struct {
			Steps []store.CollaborationStepUsage `json:"step_usage"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.Steps == nil || len(reply.Steps) != 0 {
			t.Fatal("unstarted task usage must be an empty array", w.Body.String(), err)
		}
	}
	w = clientRequest(h, "bob", "GET", "/api/collaboration/runs/"+r.ID, "")
	if w.Code != 404 {
		t.Fatal("cross-user lookup", w.Code)
	}
	raw, _ := json.Marshal(input)
	w = clientRequest(h, u.Name, "PUT", "/api/collaboration/runs/"+request, string(raw))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"replayed":true`) {
		t.Fatal("creation replay", w.Code, w.Body.String())
	}
	input.Text = "different"
	raw, _ = json.Marshal(input)
	w = clientRequest(h, u.Name, "PUT", "/api/collaboration/runs/"+request, string(raw))
	if w.Code != 409 {
		t.Fatal("changed replay", w.Code)
	}
	w = clientRequest(h, u.Name, "GET", "/api/collaboration/runs/"+r.ID+"/events?after=-1", "")
	if w.Code != 400 {
		t.Fatal("bad cursor accepted", w.Code)
	}
	id := strings.Repeat("c", 32)
	raw, _ = json.Marshal(map[string]string{"scope": s.collaborationScope(u)})
	w = clientRequest(h, u.Name, "PUT", "/api/collaboration/requests/"+id+"/abandon", string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	input.Text = "fixture task"
	raw, _ = json.Marshal(input)
	w = clientRequest(h, u.Name, "PUT", "/api/collaboration/runs/"+id, string(raw))
	if w.Code != 410 {
		t.Fatal("late creation escaped fence", w.Code, w.Body.String())
	}
}
