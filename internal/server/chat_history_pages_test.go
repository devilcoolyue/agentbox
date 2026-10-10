package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"agentbox/internal/store"
)

type historyPageView struct {
	Entries      []logEntry `json:"entries"`
	Start        int        `json:"start"`
	HasMore      bool       `json:"has_more"`
	ActiveThread string     `json:"active_thread"`
	ThreadID     string     `json:"thread_id"`
}

func smallHistoryPages(t *testing.T) {
	t.Helper()
	saved := historyPage
	historyPage = historyLimits{Entries: 5, Bytes: 1 << 20, MaxEntries: 12, MaxBytes: 1 << 20, ReloadEntries: 30, ReloadBytes: 1 << 20}
	t.Cleanup(func() { historyPage = saved })
}

func getHistory(t *testing.T, s *Server, sess store.Session, query string) (int, historyPageView) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleHistory(w, httptest.NewRequest(http.MethodGet, "/history"+query, nil), sess)
	var page historyPageView
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, page
}

// writeTurns appends turns whose agent output has the given number of
// rendered events; each is followed by a Claude tool result that history
// must not send.
func writeTurns(t *testing.T, s *Server, sess store.Session, sizes ...int) string {
	t.Helper()
	room := s.chat.room(sess.ID)
	for i, n := range sizes {
		id := fmt.Sprintf("turn-%d", i)
		if err := room.appendLog(logEntry{Kind: "user", Text: id, Turn: &chatTurnMetadata{ID: id, Model: "fixture"}}); err != nil {
			t.Fatal(err)
		}
		for j := range n {
			text := fmt.Sprintf("%s answer %d", id, j)
			for _, ev := range []string{
				fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, text),
				fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":%q}]}}`, strings.Repeat("x", 64)),
			} {
				if err := room.appendLog(logEntry{Kind: "event", Event: json.RawMessage(ev)}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return s.activeThread(sess)
}

func historyKey(e logEntry) string {
	if e.Kind == "user" {
		return "user:" + e.Text
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(e.Event, &ev)
	if ev.Type == "user" {
		return "tool_result"
	}
	if len(ev.Message.Content) == 1 {
		return ev.Message.Content[0].Text
	}
	return e.Kind
}

func TestHistoryPagesCoverThreadOnTurnBoundaries(t *testing.T) {
	smallHistoryPages(t)
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	tid := writeTurns(t, s, sess, 3, 3, 3, 3, 3)

	code, page := getHistory(t, s, sess, "")
	if code != http.StatusOK || !page.HasMore || page.ActiveThread != tid {
		t.Fatalf("newest page: code=%d %+v", code, page)
	}
	var all []string
	cursors := []int{}
	for {
		if len(page.Entries) == 0 || page.Entries[0].Kind != "user" {
			t.Fatalf("page at %d does not start a turn: %+v", page.Start, page.Entries)
		}
		keys := []string{}
		for _, e := range page.Entries {
			if k := historyKey(e); k == "tool_result" {
				t.Fatalf("tool result sent at page %d", page.Start)
			} else {
				keys = append(keys, k)
			}
		}
		all = append(keys, all...)
		cursors = append(cursors, page.Start)
		if !page.HasMore {
			break
		}
		code, page = getHistory(t, s, sess, fmt.Sprintf("?thread=%s&before=%d", tid, page.Start))
		if code != http.StatusOK || page.ThreadID != tid || page.ActiveThread != "" {
			t.Fatalf("older page: code=%d %+v", code, page)
		}
	}
	want := []string{}
	for i := range 5 {
		want = append(want, fmt.Sprintf("user:turn-%d", i))
		for j := range 3 {
			want = append(want, fmt.Sprintf("turn-%d answer %d", i, j))
		}
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("pages lost or repeated entries:\n got %v\nwant %v", all, want)
	}
	// Each page holds whole turns: 4 rendered entries per turn, a budget of 5.
	if !reflect.DeepEqual(cursors, []int{21, 7, 0}) {
		t.Fatalf("cursors = %v", cursors)
	}

	// A reconnect keeps what the client loaded and picks up new entries.
	writeTurns(t, s, sess, 1)
	code, page = getHistory(t, s, sess, fmt.Sprintf("?thread=%s&from=7", tid))
	if code != http.StatusOK || page.Start != 7 || !page.HasMore || len(page.Entries) != 4*4+2 {
		t.Fatalf("refresh from cursor: code=%d start=%d entries=%d", code, page.Start, len(page.Entries))
	}
	// A cursor for another thread, or a range past the reload cap, returns
	// the newest page instead.
	for _, query := range []string{"?thread=other&from=7", fmt.Sprintf("?thread=%s&from=0", tid), fmt.Sprintf("?thread=%s&from=999", tid)} {
		historyPage.ReloadEntries = 16
		code, page = getHistory(t, s, sess, query)
		if code != http.StatusOK || page.Start != 28 {
			t.Fatalf("%s: code=%d start=%d", query, code, page.Start)
		}
	}
}

func TestHistorySplitsOnlyOversizedTurns(t *testing.T) {
	smallHistoryPages(t)
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	tid := writeTurns(t, s, sess, 2, 20)

	_, page := getHistory(t, s, sess, "")
	head := page.Entries[0]
	if head.Kind != "turn_context" || head.Turn == nil || head.Turn.ID != "turn-1" {
		t.Fatalf("split page lost its turn: %+v", head)
	}
	if len(page.Entries) != historyPage.MaxEntries+1 || historyKey(page.Entries[1]) != "turn-1 answer 8" {
		t.Fatalf("split page = %d entries, first %q", len(page.Entries), historyKey(page.Entries[1]))
	}
	// The rest of the turn starts at its own user message; the turn before
	// it is a page of its own.
	_, older := getHistory(t, s, sess, fmt.Sprintf("?thread=%s&before=%d", tid, page.Start))
	last := older.Entries[len(older.Entries)-1]
	if historyKey(older.Entries[0]) != "user:turn-1" || historyKey(last) != "turn-1 answer 7" || !older.HasMore {
		t.Fatalf("older part: first=%q last=%q more=%v", historyKey(older.Entries[0]), historyKey(last), older.HasMore)
	}
	_, first := getHistory(t, s, sess, fmt.Sprintf("?thread=%s&before=%d", tid, older.Start))
	if first.Start != 0 || first.HasMore || len(first.Entries) != 3 || historyKey(first.Entries[0]) != "user:turn-0" {
		t.Fatalf("first page: %+v", first)
	}
}

func TestHistoryRejectsInvalidCursors(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	tid := writeTurns(t, s, sess, 1)
	for _, query := range []string{
		"?thread=" + tid + "&before=0",
		"?thread=" + tid + "&before=4",
		"?thread=" + tid + "&before=x",
		"?thread=../x&before=1",
		"?before=1",
		"?thread=20260101-000000-abcdef&before=1",
	} {
		if code, _ := getHistory(t, s, sess, query); code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d", query, code)
		}
	}
	if code, page := getHistory(t, s, sess, "?thread="+tid+"&before=3"); code != http.StatusOK || page.Start != 0 || page.HasMore || len(page.Entries) != 2 {
		t.Fatalf("whole thread: code=%d %+v", code, page)
	}
	if _, err := os.Stat(s.threadPath(sess, "20260101-000000-abcdef")); !os.IsNotExist(err) {
		t.Fatalf("older-page request created a thread: %v", err)
	}
}
