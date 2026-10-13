package server

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/chat"
	"agentbox/internal/store"
)

func (f *receiptFixture) waitState(t *testing.T, id string, done func(store.ChatRequest) bool) store.ChatRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := f.s.store.ChatRequest(f.u, f.sess.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if done(c) {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal("request never reached the expected state", c)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func stateIs(state string) func(store.ChatRequest) bool {
	return func(c store.ChatRequest) bool { return c.State == state }
}

// Follow-ups sent while a turn runs wait in order, persist as queued receipts
// every page can list and withdraw, and run one by one after the turn ends.
func TestChatFollowUpsQueueInOrderAndWithdraw(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	first := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+first, "alice", f.input))
	<-f.started
	queued, steer := f.input, f.input
	queued.Delivery, steer.Delivery = store.DeliveryQueue, store.DeliverySteer
	a, b := newOperationID(), newOperationID()
	if c := receiptFrom(t, f.request("PUT", "requests/"+a, "alice", queued)); c.State != store.ChatQueued || c.Request.Delivery != store.DeliveryQueue {
		t.Fatal(c)
	}
	// Codex takes no follow-ups mid-turn, so a steer waits in the queue too.
	if c := receiptFrom(t, f.request("PUT", "requests/"+b, "alice", steer)); c.State != store.ChatQueued {
		t.Fatal("steer did not fall back to the queue", c)
	}
	if w := f.request("PUT", "requests/"+newOperationID(), "alice", f.input); w.Code != 409 {
		t.Fatal("ordinary send accepted while busy", w.Code)
	}
	if c := receiptFrom(t, f.request("PUT", "requests/"+a, "alice", queued)); c.State != store.ChatQueued {
		t.Fatal("replay changed a queued request", c)
	}
	var list struct {
		Requests []store.ChatRequest `json:"requests"`
		Pending  *store.ChatRequest  `json:"pending"`
	}
	if w := f.request("GET", "requests?pending_only=1", "alice", nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(list.Requests) != 2 || list.Requests[0].RequestID != a || list.Requests[1].RequestID != b || list.Pending == nil || list.Pending.RequestID != first {
		t.Fatal("polling pages cannot see the queue", list)
	}
	cb, _ := f.s.store.ChatRequest(f.u, f.sess.ID, b)
	if w := f.request("POST", "requests/"+b, "bob", map[string]any{"action": "withdraw", "scope": f.input.Scope, "revision": cb.Revision}); w.Code == 200 {
		t.Fatal("another user withdrew it")
	}
	if w := f.request("POST", "requests/"+b, "alice", map[string]any{"action": "withdraw", "scope": f.input.Scope, "revision": cb.Revision}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.unblock()
	if c := f.waitState(t, a, stateIs(store.ChatCompleted)); c.TurnID == "" {
		t.Fatal(c)
	}
	f.wait(t, a)
	if c, _ := f.s.store.ChatRequest(f.u, f.sess.ID, b); c.State != store.ChatAbandoned || c.ErrorCode != "withdrawn" {
		t.Fatal("withdrawn follow-up", c)
	}
	if c, _ := f.s.store.ChatRequest(f.u, f.sess.ID, first); c.State != store.ChatCompleted {
		t.Fatal(c)
	}
	if got := f.invocations.Load(); got != 2 {
		t.Fatal("turns run", got)
	}
	// With nothing running a follow-up is simply sent.
	idle := newOperationID()
	if c := receiptFrom(t, f.request("PUT", "requests/"+idle, "alice", steer)); c.State != store.ChatAccepted {
		t.Fatal(c)
	}
	f.waitState(t, idle, stateIs(store.ChatCompleted))
}

// An unreviewed outcome holds the queue, also across a restart's dispatch,
// until the user has looked at it.
func TestChatFollowUpWaitsForReview(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, true)
	first := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+first, "alice", f.input))
	<-f.started
	queued := f.input
	queued.Delivery = store.DeliveryQueue
	a := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+a, "alice", queued))
	f.unblock()
	c := f.waitState(t, first, stateIs(store.ChatUncertain))
	f.s.dispatchQueuedChats()
	time.Sleep(50 * time.Millisecond)
	if got, _ := f.s.store.ChatRequest(f.u, f.sess.ID, a); got.State != store.ChatQueued || f.invocations.Load() != 1 {
		t.Fatal("queue ran past an unreviewed turn", got)
	}
	if w := f.request("POST", "requests/"+first, "alice", map[string]any{"action": "review", "scope": f.input.Scope, "revision": c.Revision}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.waitState(t, a, func(c store.ChatRequest) bool { return c.State != store.ChatQueued })
	f.wait(t, a)
	if f.invocations.Load() != 2 {
		t.Fatal("reviewed queue did not run")
	}
}

// A follow-up the workspace no longer admits fails at dispatch without running.
func TestChatFollowUpRefusedAtDispatch(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	f.unblock()
	stale := f.input
	stale.ThreadID, stale.Delivery = "20200101-000000-aaaaaa", store.DeliveryQueue
	id := newOperationID()
	if _, fresh, err := f.s.store.QueueChatRequest(f.u, f.sess.ID, id, stale); err != nil || !fresh {
		t.Fatal(err)
	}
	f.s.dispatchQueuedChats()
	if c := f.waitState(t, id, stateIs(store.ChatFailed)); c.ErrorCode != "chat_thread_changed" || f.invocations.Load() != 0 {
		t.Fatal(c, f.invocations.Load())
	}
}

func (f *receiptFixture) historyEntries(t *testing.T) ([]logEntry, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.s.chatsDir(f.sess), f.input.ThreadID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []logEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e logEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, string(data)
}

func steerViews(entries []logEntry) (views []chat.SteerView, at []int) {
	for i, e := range entries {
		if e.Kind == "steer" {
			var v chat.SteerView
			_ = json.Unmarshal(e.Event, &v)
			views, at = append(views, v), append(at, i)
		}
	}
	return
}

const toolStepLine = `{"type":"assistant","session_id":"provider-session","message":{"id":"msg-1","model":"claude-opus-5","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_b","name":"Bash","input":{"command":"sleep 1"}}],"usage":{"input_tokens":3,"output_tokens":2}}}`

func waitSteerable(t *testing.T, room *chatRoom) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		room.mu.Lock()
		ready := room.steer != nil
		room.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("running turn never took follow-ups")
}

// A steer is written into the running turn exactly once and shows in history
// where the CLI echoed it, not where it was sent.
func TestChatSteerReachesRunningTurn(t *testing.T) {
	f := questionFixture(t)
	stepped := make(chan struct{})
	got := make(chan map[string]any, 1)
	closed := make(chan bool, 1)
	f.cli = func(in io.Reader, out io.Writer, cmd string) {
		c := newFakeTurn(t, in, out)
		c.prompt(f.input.Text)
		c.emit(`{"type":"system","subtype":"init","session_id":"provider-session"}`,
			`{"type":"user","isReplay":true,"session_id":"provider-session","message":{"role":"user","content":"synthetic prompt"}}`,
			toolStepLine)
		close(stepped)
		got <- c.read()
		c.emit(`{"type":"user","session_id":"provider-session","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_b","content":"ok"}]}}`,
			`{"type":"user","isReplay":true,"session_id":"provider-session","message":{"role":"user","content":"use the staging host"}}`)
		c.emit(finishLines...)
		closed <- c.eof()
	}
	main := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+main, "alice", f.input))
	<-stepped
	room := f.s.chat.room(f.sess.ID)
	waitSteerable(t, room)
	in := f.input
	in.Text, in.Delivery = "use the staging host", store.DeliverySteer
	id := newOperationID()
	if c := receiptFrom(t, f.request("PUT", "requests/"+id, "alice", in)); c.State != store.ChatCompleted || c.MergedInto != main || c.TurnID != "" {
		t.Fatal("steer receipt", c)
	}
	if w := f.request("PUT", "requests/"+id, "alice", in); w.Code != 200 {
		t.Fatal("replay", w.Code)
	}
	m := <-got
	msg, _ := m["message"].(map[string]any)
	if m["type"] != "user" || m["priority"] != "next" || msg["content"] != in.Text {
		t.Fatal("steer line", m)
	}
	if c := f.waitState(t, main, stateIs(store.ChatCompleted)); c.MergedInto != "" {
		t.Fatal(c)
	}
	f.wait(t, main)
	if !<-closed {
		t.Fatal("steer written twice or stdin left open")
	}
	entries, raw := f.historyEntries(t)
	views, at := steerViews(entries)
	if len(views) != 1 || views[0] != (chat.SteerView{ID: id, Text: in.Text, State: "applied"}) {
		t.Fatal("steer history", views)
	}
	toolAt, textAt := -1, -1
	for i, e := range entries {
		switch {
		case e.Kind == "event" && strings.Contains(string(e.Event), "toolu_b") && strings.Contains(string(e.Event), `"assistant"`):
			toolAt = i
		case e.Kind == "event" && strings.Contains(string(e.Event), `"text":"ok"`):
			textAt = i
		}
	}
	if !(toolAt < at[0] && at[0] < textAt) {
		t.Fatal("steer recorded out of place", toolAt, at[0], textAt)
	}
	if strings.Contains(raw, "isReplay") {
		t.Fatal("CLI echoes leaked into history")
	}
}

// A steer the CLI never took is reported as not applied when the turn ends.
func TestChatSteerNotTakenIsReported(t *testing.T) {
	f := questionFixture(t)
	stepped := make(chan struct{})
	f.cli = func(in io.Reader, out io.Writer, cmd string) {
		c := newFakeTurn(t, in, out)
		c.prompt(f.input.Text)
		c.emit(`{"type":"user","isReplay":true,"session_id":"provider-session","message":{"role":"user","content":"synthetic prompt"}}`, toolStepLine)
		close(stepped)
		c.read()
		c.emit(finishLines...)
		c.eof()
	}
	main := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+main, "alice", f.input))
	<-stepped
	waitSteerable(t, f.s.chat.room(f.sess.ID))
	in := f.input
	in.Text, in.Delivery = "too late", store.DeliverySteer
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", in))
	f.waitState(t, main, stateIs(store.ChatCompleted))
	f.wait(t, main)
	entries, _ := f.historyEntries(t)
	if views, _ := steerViews(entries); len(views) != 1 || views[0].State != "dropped" || views[0].ID != id {
		t.Fatal(views)
	}
}
