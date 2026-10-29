package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

// activeTurn is the slot of the workspace's active thread, which tests from
// before parallel threads drive as "the room".
func (r *chatRoom) activeTurn(t testing.TB) *chatTurn {
	t.Helper()
	tid, err := r.activeThreadEnsured()
	if err != nil {
		t.Fatal(err)
	}
	return r.turn(tid)
}

// Two threads of a workspace run at once: a page may start a new thread and
// send there while the first still runs. Each turn writes its own transcript
// and stops through its own pid file, and a page that joins sees both.
func TestChatThreadsRunSideBySide(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, claudeSuggestionOutput, false)
	first := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+first, "alice", f.input))
	<-f.started

	// While it runs, a new thread opens and takes a message of its own.
	w := f.request("POST", "threads", "alice", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"created":true`) {
		t.Fatal("new thread while running", w.Code, w.Body.String())
	}
	room := f.s.chat.room(f.sess.ID)
	other := room.activeTurn(t).thread
	if other == f.input.ThreadID {
		t.Fatal("the running thread was reused as the new one")
	}
	// A prior turn and title, like the fixture's thread, keep title generation
	// off the fake CLI.
	sess, _ := f.s.store.Get(f.sess.ID)
	for _, e := range []logEntry{{Kind: "user", Text: "synthetic previous turn"}, {Kind: "title", Text: "Second"}} {
		if err := f.s.appendThreadEntry(sess, other, e); err != nil {
			t.Fatal(err)
		}
	}
	in := f.input
	in.ThreadID = other
	second := newOperationID()
	if c := receiptFrom(t, f.request("PUT", "requests/"+second, "alice", in)); c.State == store.ChatQueued {
		t.Fatal("the second thread queued behind the first", c)
	}
	waitFor(t, func() bool { return f.invocations.Load() == 2 })
	if st := room.runningStatus(); len(st) != 2 || st[f.input.ThreadID] == nil || st[other] == nil {
		t.Fatalf("running %v", st)
	}

	// A page joining now learns about both; the top level is the active one.
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/sessions/"+f.sess.ID+"/chat", http.Header{"Authorization": []string{"Bearer alice"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var status map[string]any
	if err = conn.ReadJSON(&status); err != nil {
		t.Fatal(err)
	}
	threads, _ := status["threads"].(map[string]any)
	if status["type"] != "status" || status["state"] != "running" || len(threads) != 2 {
		t.Fatalf("join status %v", status)
	}
	// Switching back to the first thread works while both run.
	if w := f.request("POST", "threads/"+f.input.ThreadID+"/activate", "alice", nil); w.Code != 200 {
		t.Fatal("switch while running", w.Code, w.Body.String())
	}

	f.unblock()
	for _, id := range []string{first, second} {
		if c := f.wait(t, id); c.State != store.ChatCompleted {
			t.Fatal(id, c)
		}
	}
	// Each CLI recorded its pid in its thread's own file.
	pids := map[string]bool{}
	f.commands.Range(func(_, v any) bool {
		for _, tid := range []string{f.input.ThreadID, other} {
			if cmd := v.(string); strings.Contains(cmd, "claude -p") && strings.Contains(cmd, agent.ChatPidFile(tid)) {
				pids[tid] = true
			}
		}
		return true
	})
	if len(pids) != 2 || agent.ChatPidFile(f.input.ThreadID) == agent.ChatPidFile(other) {
		t.Fatal("threads shared a pid file", pids)
	}
	// Both transcripts hold their own turn.
	sess, _ = f.s.store.Get(f.sess.ID)
	for _, tid := range []string{f.input.ThreadID, other} {
		users := 0
		f.s.streamThread(sess, tid, func(line []byte, whole bool) {
			if strings.Contains(string(line), `"kind":"user"`) && strings.Contains(string(line), f.input.Text) {
				users++
			}
		})
		if users != 1 {
			t.Fatalf("thread %s has %d turns of this prompt", tid, users)
		}
	}
	// A page reloading its own thread gets it, not the thread another page made
	// active; a deleted one falls back to the active thread.
	history := func(thread string) string {
		r := httptest.NewRequest("GET", "/api/sessions/"+f.sess.ID+"/history?draft_context=1&thread="+thread, nil)
		r.Header.Set("Authorization", "Bearer alice")
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		var body struct {
			Active string `json:"active_thread"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return body.Active
	}
	if got := history(other); got != other {
		t.Fatal("history ignored the page's thread", got)
	}
	// Deleting a thread tells the pages, so one showing it moves on.
	if w := f.request("DELETE", "threads/"+other, "alice", nil); w.Code != 200 {
		t.Fatal("delete idle thread", w.Code, w.Body.String())
	}
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatal("no thread_deleted event", err)
		}
		if m["type"] == "thread_deleted" {
			if m["id"] != other {
				t.Fatal(m)
			}
			break
		}
	}
	if got := history(other); got != f.input.ThreadID {
		t.Fatal("a deleted thread was served", got)
	}
}

// The workspace runs at most max_parallel_chats threads (3 by default). A plain send beyond it
// is refused; a follow-up waits in its queue and starts when a slot frees.
func TestChatParallelLimitQueuesFollowUps(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, claudeSuggestionOutput, false)
	room := f.s.chat.room(f.sess.ID)
	var busy []*chatTurn
	for i := 0; i < config.DefaultMaxParallelChats; i++ {
		other := room.turn("busy-" + string(rune('a'+i)))
		if err := other.begin(); err != nil {
			t.Fatal(err)
		}
		busy = append(busy, other)
	}
	w := f.request("PUT", "requests/"+newOperationID(), "alice", f.input)
	if w.Code != 409 {
		t.Fatal("ran past the limit", w.Code, w.Body.String())
	}
	assertProblem(t, w.Body.Bytes(), "chat_parallel_limit")

	in := f.input
	in.Delivery = store.DeliveryQueue
	id := newOperationID()
	if c := receiptFrom(t, f.request("PUT", "requests/"+id, "alice", in)); c.State != store.ChatQueued {
		t.Fatal("follow-up not queued at the limit", c)
	}
	f.unblock()
	busy[0].end() // a slot frees and the queue moves
	if c := f.wait2(t, id, room); c.State != store.ChatCompleted {
		t.Fatal(c)
	}
	for _, other := range busy[1:] {
		other.end()
	}
}

// wait2 waits for one request to finish while other slots stay held.
func (f *receiptFixture) wait2(t *testing.T, id string, room *chatRoom) store.ChatRequest {
	t.Helper()
	var c store.ChatRequest
	waitFor(t, func() bool {
		var err error
		c, err = f.s.store.ChatRequest(f.u, f.sess.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return c.State != store.ChatQueued && c.State != store.ChatAccepted && c.State != store.ChatStarting && c.State != store.ChatRunning
	})
	return c
}

// A thread's provider conversation lives in its transcript. The session keeps
// the active thread's as before, so only that thread's turn changes it.
func TestChatThreadConversationsStayApart(t *testing.T) {
	s, sess := newTestServer(t)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.chat = newChatManager(s)
	room := s.chat.room(sess.ID)
	active := room.activeTurn(t)
	other := room.turn("20261028-000000-0000aa")
	sess, _ = s.store.Get(sess.ID)
	if err := (chatRuntime{other}).ProviderSession(sess, "conv-other"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.store.Get(sess.ID); got.ChatSession != "" {
		t.Fatal("a background thread replaced the active conversation", got.ChatSession)
	}
	if err := (chatRuntime{active}).ProviderSession(sess, "conv-active"); err != nil {
		t.Fatal(err)
	}
	sess, _ = s.store.Get(sess.ID)
	if sess.ChatSession != "conv-active" {
		t.Fatal(sess.ChatSession)
	}
	if got := (chatRuntime{other}).Conversation(sess, other.thread); got != "conv-other" {
		t.Fatal("other thread resumes", got)
	}
	if got := (chatRuntime{active}).Conversation(sess, active.thread); got != "conv-active" {
		t.Fatal("active thread resumes", got)
	}
}

// max_parallel_chats is read on every admission: raising it starts a request
// queued for a free slot without waiting for a running turn to end, and
// lowering it refuses new turns while the running ones carry on.
func TestChatParallelLimitFollowsSettings(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, claudeSuggestionOutput, false)
	room := f.s.chat.room(f.sess.ID)
	var busy []*chatTurn
	for i := 0; i < config.DefaultMaxParallelChats; i++ {
		other := room.turn("busy-" + string(rune('a'+i)))
		if err := other.begin(); err != nil {
			t.Fatal(err)
		}
		busy = append(busy, other)
	}
	in := f.input
	in.Delivery = store.DeliveryQueue
	id := newOperationID()
	if c := receiptFrom(t, f.request("PUT", "requests/"+id, "alice", in)); c.State != store.ChatQueued {
		t.Fatal("follow-up not queued at the limit", c)
	}
	// The kick after queueing finds every slot taken; it must be done before
	// the limit grows, or it would start the request by itself.
	time.Sleep(300 * time.Millisecond)
	if c, err := f.s.store.ChatRequest(f.u, f.sess.ID, id); err != nil || c.State != store.ChatQueued {
		t.Fatal("follow-up left the queue with every slot taken", c.State, err)
	}
	four := int64(4)
	if err := f.s.cfg.ApplySettings(config.SettingsPatch{MaxParallelChats: &four}); err != nil {
		t.Fatal(err)
	}
	f.s.chat.parallelLimitChanged()
	f.unblock()
	if c := f.wait2(t, id, room); c.State != store.ChatCompleted {
		t.Fatal("raised limit did not start the queued request", c)
	}

	one := int64(1)
	if err := f.s.cfg.ApplySettings(config.SettingsPatch{MaxParallelChats: &one}); err != nil {
		t.Fatal(err)
	}
	if err := room.turn("late").begin(); err != errChatParallel {
		t.Fatal("ran past a lowered limit", err)
	}
	for _, other := range busy {
		if !room.threadRunning(other.thread) {
			t.Fatal("lowering the limit stopped a running turn")
		}
		other.end()
	}
	if err := room.turn("late").begin(); err != nil {
		t.Fatal("a free slot under the lowered limit was refused", err)
	}
	room.turn("late").end()
}
