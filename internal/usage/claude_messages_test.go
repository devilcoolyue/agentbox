package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func messageLine(id, model, stop string, input, output, read, write int) string {
	return fmt.Sprintf(`{"type":"assistant","entrypoint":"sdk-cli","message":{"id":%q,"model":%q,"stop_reason":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`, id, model, stop, input, output, read, write)
}
func chatBase(turn string) store.UsageEvent {
	return store.UsageEvent{User: "alice", SessionID: "s1", ThreadID: "thread", TurnID: turn, Agent: "claude", Kind: store.UsageKindChat}
}
func observe(tally *Tally, turn string, lines ...string) {
	for _, line := range lines {
		tally.Observe(chatBase(turn), []byte(line))
	}
}
func TestClaudeMessageSelectionAndResumedBilling(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 10}}}
	if _, err := s.store.Grant("alice", 1000000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	tally := s.NewTally()
	final := messageLine("msg-one", "main", "end_turn", 8, 100, 1200, 30)
	observe(&tally, "one", messageLine("msg-one", "main", "", 8, 2, 1200, 30), final, final,
		messageLine("msg-one", "main", "", 8, 999, 1200, 30), // completion wins even over larger placeholders
		`{"type":"result","total_cost_usd":99,"modelUsage":{"main":{"inputTokens":9999,"outputTokens":999999,"costUSD":99}}}`)
	if n := s.Flush(&tally, time.Second); n != 1 {
		t.Fatal(n)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	if len(rows) != 1 || rows[0].OutputTokens != 100 || rows[0].CostMicroUSD != 3440 || rows[0].Price.Source != "table" {
		t.Fatalf("wrong first turn: %+v", rows)
	}
	// A new tally/process replays an old message and sees one new request.
	second := s.NewTally()
	observe(&second, "two", final, messageLine("msg-two", "main", "end_turn", 2, 10, 100, 0))
	if n := s.Flush(&second, time.Second); n != 1 {
		t.Fatal(n)
	}
	if n := s.Flush(&second, time.Second); n != 0 {
		t.Fatal("double flush", n)
	}
	rows = s.store.ListUsage(store.UsageFilter{User: "alice"})
	if len(rows) != 2 || rows[0].OutputTokens != 10 || rows[0].CostMicroUSD != 310 {
		t.Fatalf("replayed usage charged: %+v", rows)
	}
	q, _ := s.store.GetQuota("alice")
	if q.BalanceMicroUSD != 1000000-3440-310 {
		t.Fatal(q)
	}
	failed := s.NewTally()
	observe(&failed, "failed", `{"type":"result","is_error":true,"usage":{"output_tokens":0},"total_cost_usd":99,"modelUsage":{"main":{"outputTokens":999999,"costUSD":99}}}`)
	if n := s.Flush(&failed, time.Second); n != 0 {
		t.Fatal("failed turn charged history", n)
	}
	if len(s.store.ListUsage(store.UsageFilter{User: "alice"})) != 2 {
		t.Fatal("failure added row")
	}
	if evs, _ := ParseUsage([]byte(claudeResultSample), chatBase("unsafe")); len(evs) != 0 {
		t.Fatal("chat result fallback still enabled")
	}
}

func TestClaudeStreamUsageUsesFinalOutputAndIndependentSubagents(t *testing.T) {
	c := newClaudeMeter()
	for _, line := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"main","model":"opus","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":100}}}}`,
		`{"type":"stream_event","parent_tool_use_id":"tool-a","event":{"type":"message_start","message":{"id":"child","model":"haiku","usage":{"input_tokens":3,"output_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":20}}}`,
		`{"type":"stream_event","parent_tool_use_id":"tool-a","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}}`,
	} {
		c.observe(chatBase("turn"), []byte(line))
	}
	if len(c.messages) != 2 || c.messages["main"].Usage.Output != 40 || c.messages["main"].Usage.CacheRead != 100 || c.messages["child"].Usage.Output != 7 {
		t.Fatal(c.messages)
	}
}

func appendTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := fmt.Fprintln(f, line); err != nil {
			t.Fatal(err)
		}
	}
}
func TestClaudeTranscriptCompletesOnlyCurrentTurnAndModels(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Output: 2}}}
	root := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace")
	main := filepath.Join(root, "provider-session.jsonl")
	appendTranscript(t, main, messageLine("old", "opus", "end_turn", 2, 100000, 0, 0))
	tally := s.NewChatTally(sess)
	observe(&tally, "one", `{"type":"system","session_id":"provider-session"}`, messageLine("new", "opus", "", 2, 2, 0, 0))
	appendTranscript(t, main, messageLine("new", "opus", "end_turn", 2, 200, 0, 0), messageLine("new", "opus", "", 2, 1, 0, 0),
		`{"type":"assistant","entrypoint":"cli","message":{"id":"terminal","model":"opus","usage":{"output_tokens":9999}}}`)
	appendTranscript(t, filepath.Join(root, "provider-session", "subagents", "agent-a.jsonl"), messageLine("child", "haiku", "end_turn", 3, 20, 0, 0))
	appendTranscript(t, filepath.Join(root, "unrelated.jsonl"), messageLine("other", "opus", "end_turn", 3, 100000, 0, 0))
	if n := s.Flush(&tally, time.Second); n != 2 {
		t.Fatal(n)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	var output, cost int64
	for _, r := range rows {
		output += r.OutputTokens
		cost += r.CostMicroUSD
		if !r.Price.PerRequest {
			t.Fatal("missing pricing semantics")
		}
	}
	if output != 220 || cost != 440 {
		t.Fatalf("output=%d cost=%d", output, cost)
	}
	// Next turn reads only appended bytes, not all previous transcript messages.
	next := s.NewChatTally(sess)
	observe(&next, "two", `{"type":"result","session_id":"provider-session","modelUsage":{"opus":{"outputTokens":100200,"costUSD":100}}}`)
	if n := s.Flush(&next, time.Second); n != 0 {
		t.Fatal("history rebilled", n)
	}
}
func TestClaudeTranscriptRefusesSymlinksAndTruncation(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	root := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace")
	main := filepath.Join(root, "provider-session.jsonl")
	appendTranscript(t, main, messageLine("old", "opus", "end_turn", 2, 100000, 0, 0))
	tally := s.NewChatTally(sess)
	observe(&tally, "one", `{"type":"system","session_id":"provider-session"}`)
	if err := os.WriteFile(main, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if n := s.Flush(&tally, time.Second); n != 0 {
		t.Fatal(n)
	}
	next := s.NewChatTally(sess)
	observe(&next, "two", `{"type":"system","session_id":"provider-session"}`)
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	appendTranscript(t, outside, messageLine("escape", "opus", "end_turn", 1, 100, 0, 0))
	if err := os.Remove(main); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, main); err != nil {
		t.Fatal(err)
	}
	if n := s.Flush(&next, time.Second); n != 0 {
		t.Fatal("followed symlink", n)
	}
}

func TestClaudeTerminalMessageIDWinsOverRequestIDAndPlaceholders(t *testing.T) {
	lines := []string{
		`{"type":"user","entrypoint":"cli","uuid":"turn"}`,
		strings.Replace(messageLine("msg-one", "opus", "", 1, 2, 100, 0), `"sdk-cli"`, `"cli"`, 1),
		strings.Replace(messageLine("msg-one", "opus", "end_turn", 1, 50, 100, 0), `"sdk-cli"`, `"cli"`, 1),
		strings.Replace(messageLine("msg-one", "opus", "", 1, 1, 100, 0), `"sdk-cli"`, `"cli"`, 1),
	}
	turns, err := parseTerminalReader(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil || len(turns) != 1 || turns[0].ev.OutputTokens != 50 || turns[0].ev.CacheReadTokens != 100 {
		t.Fatalf("turns=%+v err=%v", turns, err)
	}
}

func TestClaudeRequestPricingDoesNotUseAggregateContext(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 1}, LongContextOver: 100, Long: &config.TokenRates{Input: 2}}}
	tally := s.NewTally()
	observe(&tally, "one", messageLine("a", "opus", "end_turn", 80, 0, 0, 0), messageLine("b", "opus", "end_turn", 80, 0, 0, 0))
	if n := s.Flush(&tally, time.Second); n != 1 {
		t.Fatal(n)
	}
	row := s.store.ListUsage(store.UsageFilter{User: "alice"})[0]
	if row.InputTokens != 160 || row.CostMicroUSD != 160 {
		t.Fatalf("aggregate prompt applied long tier: %+v", row)
	}
}

func TestClaudeTranscriptSkipsPreexistingPartialLine(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	path := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace", "provider-session.jsonl")
	appendTranscript(t, path)
	if err := os.WriteFile(path, []byte(`{"type":"assistant"`), 0600); err != nil {
		t.Fatal(err)
	}
	tally := s.NewChatTally(sess)
	observe(&tally, "one", `{"type":"system","session_id":"provider-session"}`)
	appendTranscript(t, path, `}`, messageLine("new", "opus", "end_turn", 2, 50, 0, 0))
	if n := s.Flush(&tally, time.Second); n != 1 {
		t.Fatal(n)
	}
	if row := s.store.ListUsage(store.UsageFilter{User: "alice"})[0]; row.OutputTokens != 50 {
		t.Fatal(row)
	}
}
