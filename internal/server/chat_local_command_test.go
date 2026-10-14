package server

import (
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// What Claude Code prints for /context: one synthetic answer and a result
// without any model round-trip.
const claudeLocalCommandOutput = `{"type":"system","subtype":"init","session_id":"provider-session"}
{"type":"assistant","session_id":"provider-session","parent_tool_use_id":null,"message":{"id":"local-1","model":"<synthetic>","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"## Context Usage"}],"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"result","subtype":"success","is_error":false,"num_turns":0,"duration_api_ms":0,"total_cost_usd":0,"session_id":"provider-session","result":"## Context Usage","modelUsage":{}}
`

// A local slash command as a thread's first message neither names the thread
// nor counts as its opening turn; the next real message still gets a title.
func TestChatLocalCommandDoesNotNameThread(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, claudeLocalCommandOutput, false)
	tid := newThreadID(f.sess.CreatedAt.Add(1))
	if err := f.s.setActiveThread(f.sess, tid); err != nil {
		t.Fatal(err)
	}
	f.input.ThreadID, f.input.Text = tid, "/context"
	f.unblock()
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	if c := f.wait(t, id); c.State != store.ChatCompleted {
		t.Fatal(c)
	}
	closeTestServer(t, f.s) // waits for a title task, had one been spawned
	f.commands.Range(func(_, v any) bool {
		if cmd := v.(string); strings.Contains(cmd, "--output-format json") {
			t.Errorf("title generated for a local command: %q", cmd)
		}
		return true
	})
	entries, _ := f.historyEntries(t)
	local := false
	for _, e := range entries {
		if e.Kind == "status" && e.State == "idle" {
			local = e.Local
		}
	}
	if !local {
		t.Fatal("idle status not marked local", entries)
	}
	if first, turns, titled := f.s.threadTitleSource(f.sess, tid); first != "" || turns != 0 || titled {
		t.Fatal("local turn counted for the title", first, turns, titled)
	}
	if rows := f.s.store.ListUsage(store.UsageFilter{User: f.u.Name}); len(rows) != 0 {
		t.Fatal("local command metered", rows)
	}
}

func TestThreadTitleSourceSkipsLocalTurns(t *testing.T) {
	s, sess := newTestServer(t)
	if err := s.migrateThreads(sess); err != nil {
		t.Fatal(err)
	}
	tid, err := s.ensureActiveThread(sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []logEntry{
		{Kind: "user", Text: "/context"},
		{Kind: "status", State: "idle", Local: true},
		{Kind: "user", Text: "fix the login bug"},
		{Kind: "status", State: "idle"},
		{Kind: "user", Text: "/cost"},
		{Kind: "status", State: "idle", Local: true},
	} {
		if err := s.appendThreadEntry(sess, tid, e); err != nil {
			t.Fatal(err)
		}
	}
	if first, turns, titled := s.threadTitleSource(sess, tid); first != "fix the login bug" || turns != 1 || titled {
		t.Fatal(first, turns, titled)
	}
}
