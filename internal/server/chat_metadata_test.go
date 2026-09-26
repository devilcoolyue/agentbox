package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentbox/internal/agent"
	"github.com/gorilla/websocket"
)

func TestChatMetadataLiveAndHistory(t *testing.T) {
	s, sess := newTestServer(t)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.chat = newChatManager(s)
	room := s.chat.room(sess.ID)
	options, err := agent.ResolveTurnOptions("claude", "fixture", "high", "budget", nil)
	if err != nil {
		t.Fatal(err)
	}
	turns := []chatTurnMetadata{
		{ID: "native", Model: "fixture", Effort: "high", Control: "effort"},
		{ID: "budget", Model: "fixture", Effort: options.Effort, Control: options.Control, BudgetTokens: options.BudgetTokens},
		{ID: "default", Model: "fixture", Effort: "", Control: "effort"},
		{ID: "unsupported", Model: "fixture", Effort: "", Unsupported: true},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		cw := &connWriter{c: conn}
		room.attach(cw)
		defer room.detach(cw)
		for _, turn := range turns {
			room.recordChat(logEntry{Kind: "user", Text: "fixture prompt", Turn: &turn}, "user_message", "")
		}
		room.recordChat(logEntry{Kind: "event", Event: json.RawMessage(`{"type":"assistant","message":{"model":"reported-model","content":[{"type":"text","text":"answer"}]}}`)}, "agent_event", "")
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	live := make([]logEntry, 0, len(turns)+1)
	for range len(turns) + 1 {
		var message struct {
			logEntry
			Type string `json:"type"`
		}
		if err := conn.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		if message.TS.IsZero() || message.Type == "" {
			t.Fatalf("missing live timestamp/type: %+v", message)
		}
		live = append(live, message.logEntry)
	}
	if live[1].Turn.BudgetTokens != 24000 {
		t.Fatalf("budget snapshot = %+v", live[1].Turn)
	}
	// Changing today's model settings must not rewrite a historical request.
	sess.DefaultModel = "other-model"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleHistory(w, httptest.NewRequest(http.MethodGet, "/history", nil), sess)
	var history struct {
		Entries []logEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, history.Entries) {
		t.Fatalf("live/history differ:\nlive=%+v\nhistory=%s", live, w.Body.String())
	}
	for i, turn := range turns {
		if !reflect.DeepEqual(*history.Entries[i].Turn, turn) {
			t.Fatalf("turn %d changed", i)
		}
	}
}

func TestHistoryWindowRetainsTurnMetadata(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	room := s.chat.room(sess.ID)
	room.recordChat(logEntry{Kind: "user", Text: "large prompt", Turn: &chatTurnMetadata{ID: "long-turn", Model: "fixture", Effort: "low", Control: "effort"}}, "user_message", "")
	path := s.threadPath(sess, s.activeThread(sess))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(strings.Repeat("{\"kind\":\"event\",\"ts\":\"2026-09-26T00:00:00Z\",\"event\":{\"type\":\"turn.started\"}}\n", 2005))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleHistory(w, httptest.NewRequest(http.MethodGet, "/history", nil), sess)
	var history struct {
		Entries []logEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Entries) != 2001 {
		t.Fatalf("entries = %d", len(history.Entries))
	}
	head := history.Entries[0]
	if head.Kind != "turn_context" || head.Text != "" || head.Turn == nil || head.Turn.ID != "long-turn" {
		t.Fatalf("lost context: %+v", head)
	}
}
