package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

// A Claude turn whose five-hour window reaches 95% in its second request: the
// pause stops it at the third request, after the tool result is in.
const claudeFiveHourOutput = `{"type":"system","subtype":"init","session_id":"provider-session"}
{"type":"system","subtype":"status","status":"requesting","session_id":"provider-session"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":4102444800,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.94,"resetsAt":4102444800},"seven_day":{"utilization":0.3,"resetsAt":4102444800}}},"session_id":"provider-session"}
{"type":"assistant","session_id":"provider-session","parent_tool_use_id":null,"message":{"id":"m1","model":"claude-opus-5","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"user","session_id":"provider-session","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"}]}}
{"type":"system","subtype":"status","status":"requesting","session_id":"provider-session"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":4102444800,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.95,"resetsAt":4102444800}}},"session_id":"provider-session"}
{"type":"assistant","session_id":"provider-session","parent_tool_use_id":null,"message":{"id":"m2","model":"claude-opus-5","role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"user","session_id":"provider-session","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"b"}]}}
{"type":"system","subtype":"status","status":"requesting","session_id":"provider-session"}
{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":3,"session_id":"provider-session","result":""}
`

func TestFiveHourPauseStopsTurnQueuesResumeAndHoldsUntilReset(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, claudeFiveHourOutput, false)
	if err := f.s.store.SetKeepalive(f.sess.ID, store.WorkspaceKeepalive{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	f.input.Tasks, f.input.Suggestions = true, true
	f.unblock()
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	c := f.wait(t, id)
	if c.State != store.ChatInterrupted || c.ErrorCode != "chat_quota_paused" {
		t.Fatal("paused turn:", c.State, c.ErrorCode)
	}
	waitFor(t, func() bool { return f.interruptions.Load() == 1 })
	entries, _ := f.historyEntries(t)
	last := entries[len(entries)-1]
	if last.Kind != "status" || last.ProblemDetails == nil || last.ProblemDetails.Code != "chat_quota_paused" {
		t.Fatalf("paused turn's status: %+v", last)
	}
	pause, paused := f.s.quotaPaused(f.sess.ID)
	if !paused || pause.Percent != 95 || pause.Threshold != config.DefaultPausePercent || pause.ResetsAt != 4102444800000 {
		t.Fatal("pause:", pause, paused)
	}

	// The "继续" waits in the queue with the stopped request's settings.
	var resume store.ChatRequest
	waitFor(t, func() bool {
		var err error
		resume, _, err = f.s.store.NextQueuedChatRequest(f.sess.ID)
		return err == nil
	})
	in := resume.Request
	if in.Text != quotaResumeText || in.Delivery != store.DeliveryQueue || in.Model != f.input.Model ||
		!in.Tasks || !in.Suggestions || in.ThreadID != f.input.ThreadID || in.Scope != f.input.Scope {
		t.Fatalf("resume request: %+v", in)
	}
	f.s.chat.room(f.sess.ID).dispatchQueue()
	if again, _ := f.s.store.ChatRequest(f.u, f.sess.ID, resume.RequestID); again.State != store.ChatQueued {
		t.Fatal("the pause let the queue run:", again.State)
	}

	// An ordinary send is refused; a queued one waits for the reset.
	w := f.request("PUT", "requests/"+newOperationID(), "alice", f.input)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "chat_quota_reserved") {
		t.Fatal("ordinary send while paused:", w.Code, w.Body.String())
	}
	queued := f.input
	queued.Delivery = store.DeliveryQueue
	if c := receiptFrom(t, f.request("PUT", "requests/"+newOperationID(), "alice", queued)); c.State != store.ChatQueued {
		t.Fatal("queued send while paused:", c.State)
	}

	// Turning the pause off ends it at once.
	put := func(body any) *http.Response {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("PUT", "/api/sessions/"+f.sess.ID+"/keepalive", strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer alice")
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		return w.Result()
	}
	if res := put(map[string]any{"enabled": true, "pause_percent": 42}); res.StatusCode != http.StatusBadRequest {
		t.Fatal("threshold out of range accepted:", res.StatusCode)
	}
	if res := put(map[string]any{"enabled": true, "pause_percent": 96}); res.StatusCode != http.StatusOK {
		t.Fatal(res.StatusCode)
	}
	if _, paused := f.s.quotaPaused(f.sess.ID); paused {
		t.Fatal("a 96% threshold still pauses at 95%")
	}
	if res := put(map[string]any{"enabled": true}); res.StatusCode != http.StatusOK {
		t.Fatal(res.StatusCode)
	}
	if k, _ := f.s.store.Keepalive(f.sess.ID); k.PausePercent != 96 {
		t.Fatal("a page without the field reset the threshold:", k.PausePercent)
	}

	// The reset lets the queue run: the "继续" goes first.
	if res := put(map[string]any{"enabled": true, "pause_percent": 0}); res.StatusCode != http.StatusOK {
		t.Fatal(res.StatusCode)
	}
	if _, paused := f.s.quotaPaused(f.sess.ID); !paused {
		t.Fatal("back to the default threshold, the pause is gone")
	}
	f.input.Text = quotaResumeText // the fixture checks the prompt it receives
	f.s.quota.set("shared", store.RateWindow{Percent: 95, ResetsAt: time.Now().Add(-time.Second)})
	f.s.dispatchQueuedChats()
	waitFor(t, func() bool { return f.invocations.Load() == 2 })
	if c := f.wait(t, resume.RequestID); c.State != store.ChatInterrupted || c.ErrorCode != "chat_quota_paused" {
		t.Fatal("resumed turn (its output pauses again):", c.State, c.ErrorCode)
	}
}

func TestFiveHourWindowsOutliveRestart(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent, sess.AccountID = config.AgentClaude, "acct"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetKeepalive(sess.ID, store.WorkspaceKeepalive{Enabled: true, PausePercent: 90}); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if !s.noteFiveHour(sess, agent.RateWindow{Percent: 91, ResetsAt: reset}) {
		t.Fatal("91% did not reach a 90% threshold")
	}
	if s.noteFiveHour(sess, agent.RateWindow{Percent: 80, ResetsAt: time.Now().Add(-time.Minute)}) {
		t.Fatal("a window already over counted")
	}
	s.quota = quotaWindows{} // what a restart forgets
	if _, paused := s.quotaPause(sess); paused {
		t.Fatal("pause without a window")
	}
	s.loadQuotaWindows()
	p, paused := s.quotaPause(sess)
	if !paused || p.Percent != 91 || p.Threshold != 90 || p.ResetsAt != reset.UnixMilli() {
		t.Fatal("restored pause:", p, paused)
	}
	// Only a workspace that keeps caches pauses, and -1 turns it off.
	for _, k := range []store.WorkspaceKeepalive{{Enabled: false}, {Enabled: true, PausePercent: -1}} {
		if err := s.store.SetKeepalive(sess.ID, k); err != nil {
			t.Fatal(err)
		}
		if _, paused := s.quotaPause(sess); paused {
			t.Fatalf("%+v paused", k)
		}
	}
	if _, _, err := s.store.NextQueuedChatRequest(sess.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}
