package server

import (
	"agentbox/internal/store"
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCollaborationStreamReadOnlyOwnershipAndDisconnect(t *testing.T) {
	s, source, h := clientTestServer(t)
	u, _ := s.store.GetUser(source.User)
	if err := s.cfg.SetCollaborationEnabled(true); err != nil {
		t.Fatal(err)
	}
	roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}, Executor: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}}
	input := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: source.ID, Repository: ".", Baseline: strings.Repeat("a", 40), Text: "fixture", Roles: roles, MaxSteps: 20}
	r, _, err := s.store.CreateCollaboration(u, store.NewCollaborationID(), input, store.CollaborationConfig{Roles: roles, MaxSteps: 20})
	if err != nil {
		t.Fatal(err)
	}
	if response := clientRequest(h, "bob", "GET", "/api/collaboration/runs/"+r.ID+"/stream", ""); response.Code != 404 {
		t.Fatal(response.Code)
	}
	front := httptest.NewServer(h)
	defer front.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", front.URL+"/api/collaboration/runs/"+r.ID+"/stream", nil)
	request.Header.Set("Authorization", "Bearer client-fixture-"+u.Name)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		raw, _ := io.ReadAll(response.Body)
		t.Fatal(response.StatusCode, string(raw))
	}
	scan := bufio.NewScanner(response.Body)
	if !scan.Scan() || !strings.Contains(scan.Text(), "connected") {
		t.Fatal("missing handshake")
	}
	s.emitCollaboration(r.ID, map[string]any{"version": 1, "run_id": r.ID, "step_id": "step", "event": map[string]string{"text": "fixture"}})
	found := false
	for scan.Scan() {
		if strings.Contains(scan.Text(), "fixture") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("live frame missing", scan.Err())
	}
	current, err := s.store.Collaboration(u, r.ID)
	if err != nil || current.Revision != r.Revision || current.State != store.CollabAccepted {
		t.Fatal("stream scheduled work", current, err)
	}
	cancel()
	response.Body.Close()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.collaborationJobs.mu.Lock()
		n := len(s.collaborationJobs.listeners[r.ID])
		s.collaborationJobs.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("subscriber leaked")
		case <-tick.C:
		}
	}
}
