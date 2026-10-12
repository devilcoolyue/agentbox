package server

import (
	"agentbox/internal/store"
	"encoding/json"
	"strings"
	"testing"
)

func TestCollaborationThreadContextAndStableProjection(t *testing.T) {
	s, sess, h := clientTestServer(t)
	u, _ := s.store.GetUser(sess.User)
	if err := s.cfg.SetCollaborationEnabled(true); err != nil {
		t.Fatal(err)
	}
	w := clientRequest(h, u.Name, "GET", "/api/sessions/"+sess.ID+"/history?draft_context=1", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var history struct {
		ActiveThread string `json:"active_thread"`
	}
	json.Unmarshal(w.Body.Bytes(), &history)
	tid := history.ActiveThread
	room := s.chat.room(sess.ID)
	room.fileMu.Lock()
	for _, entry := range []logEntry{{Kind: "user", Text: "Remember the requirement"}, {Kind: "event", Event: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"Agreed"},{"type":"tool_use","input":{"text":"not visible"}}]}}`)}} {
		if err := s.appendThreadEntry(sess, tid, entry); err != nil {
			t.Fatal(err)
		}
	}
	room.fileMu.Unlock()
	w = clientRequest(h, u.Name, "GET", "/api/sessions/"+sess.ID+"/collaboration/context?thread="+tid, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var c struct {
		Context store.CollaborationThreadContext `json:"context"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Context.Anchor != 3 || !strings.Contains(c.Context.Text, "Agreed") || strings.Contains(c.Context.Text, "secret") || strings.Contains(c.Context.Text, "not visible") {
		t.Fatal(c)
	}
	if err := s.validateCollaborationContext(sess, &c.Context); err != nil {
		t.Fatal(err)
	}
	tampered := c.Context
	tampered.Text += "forged"
	if s.validateCollaborationContext(sess, &tampered) == nil {
		t.Fatal("accepted client supplied context")
	}
	roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}, Executor: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "fixture"}}
	in := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: sess.ID, Repository: ".", Baseline: strings.Repeat("a", 40), Text: "Build it", Roles: roles, MaxSteps: 20, Context: &c.Context}
	run, _, err := s.store.CreateCollaboration(u, strings.Repeat("b", 32), in, store.CollaborationConfig{Roles: roles, MaxSteps: 20})
	if err != nil {
		t.Fatal(err)
	}
	w = clientRequest(h, u.Name, "GET", "/api/sessions/"+sess.ID+"/history?draft_context=1", "")
	if w.Code != 200 || strings.Count(w.Body.String(), run.ID) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = clientRequest(h, u.Name, "DELETE", "/api/sessions/"+sess.ID+"/chat/threads/"+tid, "")
	if w.Code != 409 {
		t.Fatal("orphaned task", w.Code, w.Body.String())
	}
	meta, ok := s.scanThread(sess, tid)
	if !ok || meta.Turns != 2 {
		t.Fatal(meta)
	}
	room.fileMu.Lock()
	s.appendThreadEntry(sess, tid, logEntry{Kind: "user", Text: "later"})
	room.fileMu.Unlock()
	if s.validateCollaborationContext(sess, &c.Context) == nil {
		t.Fatal("accepted stale preview")
	}
	entries := parseEntries([]byte(`{"kind":"draft_context"}
{"kind":"user","text":"a"}
{"kind":"user","text":"b"}
{"kind":"user","text":"c"}`))
	page, err := s.collaborationHistory(sess, tid, entries, 0, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(page)
	if strings.Contains(string(b), run.ID) {
		t.Fatal("anchor belongs to next page")
	}
	page, err = s.collaborationHistory(sess, tid, entries, 3, 4, true)
	b, _ = json.Marshal(page)
	if err != nil || strings.Count(string(b), run.ID) != 1 {
		t.Fatal(err, string(b))
	}
}

func TestThreadContextBoundedAndAttributed(t *testing.T) {
	raw := []byte("{\"kind\":\"user\",\"text\":\"" + strings.Repeat("x", 25000) + "\"}\n{\"kind\":\"user\",\"text\":\"recent\"}")
	entries := parseEntries(raw)
	c := threadContext(entries, "thread", 2, true)
	if !c.Truncated || c.Text != "User: recent" || len(c.SHA256) != 64 {
		t.Fatal(c)
	}
	no := threadContext(entries, "thread", 2, false)
	if no.Text != "" || no.Truncated || no.SHA256 == c.SHA256 {
		t.Fatal(no)
	}
}
