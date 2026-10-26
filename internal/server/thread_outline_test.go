package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"agentbox/internal/store"
)

type outlineView struct {
	ThreadID string        `json:"thread_id"`
	Items    []outlineItem `json:"items"`
}

func getOutline(t *testing.T, s *Server, sess store.Session, query string) (int, outlineView) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleOutline(w, httptest.NewRequest(http.MethodGet, "/outline"+query, nil), sess)
	var view outlineView
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, view
}

func appendEntries(t *testing.T, s *Server, sess store.Session, entries ...logEntry) {
	t.Helper()
	room := s.chat.room(sess.ID)
	for _, e := range entries {
		if err := room.appendLog(e); err != nil {
			t.Fatal(err)
		}
	}
}

func eventEntry(ev string) logEntry { return logEntry{Kind: "event", Event: json.RawMessage(ev)} }

func outlinePairs(items []outlineItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Text+" => "+it.Answer)
	}
	return out
}

func TestOutlineListsMessagesWithAnswers(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	appendEntries(t, s, sess,
		// Claude: the result replaces the interim text before it.
		logEntry{Kind: "user", Text: "  why does it fail?  "},
		eventEntry(`{"type":"assistant","message":{"content":[{"type":"text","text":"Let me look."},{"type":"tool_use","name":"Read"}]}}`),
		eventEntry(`{"type":"user","message":{"content":[{"type":"tool_result","content":"result says assistant"}]}}`),
		eventEntry(`{"type":"result","subtype":"success","result":"It times out. Raise the limit."}`),
		logEntry{Kind: "status", State: "idle"},
		// Interrupted: the last assistant text stands in.
		logEntry{Kind: "user", Text: "stop it"},
		eventEntry(`{"type":"assistant","message":{"content":[{"type":"text","text":"First."}]}}`),
		eventEntry(`{"type":"assistant","message":{"content":[{"type":"text","text":"Stopping now."}]}}`),
		logEntry{Kind: "status", State: "interrupted"},
		// A slash command the CLI ran itself is not a message.
		logEntry{Kind: "user", Text: "/context"},
		eventEntry(`{"type":"assistant","message":{"content":[{"type":"text","text":"Context usage"}]}}`),
		logEntry{Kind: "status", State: "idle", Local: true},
		// Codex: the last agent message is the answer, later commentary loses to it only if interim.
		logEntry{Kind: "user", Text: "codex turn"},
		eventEntry(`{"item":{"type":"agent_message","text":"Checking the logs."},"type":"item.completed"}`),
		eventEntry(`{"item":{"type":"agent_message","text":"Fixed it."},"type":"item.completed"}`),
		eventEntry(`{"type":"assistant","message":{"content":[{"type":"text","text":"late interim"}]}}`),
		// No answer yet.
		logEntry{Kind: "user", Text: strings.Repeat("长", outlineTextRunes+10)},
	)
	tid := s.activeThread(sess)
	code, view := getOutline(t, s, sess, "")
	if code != http.StatusOK || view.ThreadID != tid {
		t.Fatalf("code=%d %+v", code, view)
	}
	want := []string{
		"why does it fail? => It times out. Raise the limit.",
		"stop it => Stopping now.",
		"codex turn => Fixed it.",
		strings.Repeat("长", outlineTextRunes) + "… => ",
	}
	if got := outlinePairs(view.Items); !reflect.DeepEqual(got, want) {
		t.Fatalf("outline:\n got %q\nwant %q", got, want)
	}
	for i, it := range view.Items {
		if it.TS.IsZero() {
			t.Fatalf("item %d has no timestamp", i)
		}
	}

	// The cached excerpts follow appends: the open turn gets its answer.
	appendEntries(t, s, sess, eventEntry(`{"type":"result","subtype":"success","result":"Now answered."}`))
	_, view = getOutline(t, s, sess, "?thread="+tid)
	if got := view.Items[3].Answer; got != "Now answered." {
		t.Fatalf("answer after append = %q", got)
	}
}

func TestOutlineLineIsHistoryCursor(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	smallHistoryPages(t)
	tid := writeTurns(t, s, sess, 3, 3, 3, 3, 3)
	_, view := getOutline(t, s, sess, "")
	if len(view.Items) != 5 {
		t.Fatalf("items = %+v", view.Items)
	}
	for i, it := range view.Items {
		if it.Text != fmt.Sprintf("turn-%d", i) || it.Answer != fmt.Sprintf("turn-%d answer 2", i) {
			t.Fatalf("item %d = %+v", i, it)
		}
		// Jumping to an unloaded message reloads history from its line.
		code, page := getHistory(t, s, sess, fmt.Sprintf("?thread=%s&from=%d", tid, it.Line))
		if code != http.StatusOK || page.Start != it.Line || len(page.Entries) == 0 ||
			page.Entries[0].Kind != "user" || page.Entries[0].Text != it.Text || !page.Entries[0].TS.Equal(it.TS) {
			t.Fatalf("history from item %d line %d: code=%d start=%d first=%+v", i, it.Line, code, page.Start, page.Entries)
		}
	}
}

func TestOutlineThreads(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if code, view := getOutline(t, s, sess, ""); code != http.StatusOK || len(view.Items) != 0 {
		t.Fatalf("empty session: code=%d %+v", code, view)
	}
	if code, _ := getOutline(t, s, sess, "?thread=../x"); code != http.StatusBadRequest {
		t.Fatalf("bad thread id: code=%d", code)
	}
	if code, view := getOutline(t, s, sess, "?thread=t-missing"); code != http.StatusOK || len(view.Items) != 0 || view.ThreadID != "t-missing" {
		t.Fatalf("missing thread: code=%d %+v", code, view)
	}
}
