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
	// Subagent requests reach us only through their transcript; its requestId
	// is what the CLI's telemetry names.
	child := strings.Replace(messageLine("child", "haiku", "end_turn", 3, 20, 0, 0), `"entrypoint":"sdk-cli",`, `"entrypoint":"sdk-cli","requestId":"req_child",`, 1)
	appendTranscript(t, filepath.Join(root, "provider-session", "subagents", "agent-a.jsonl"), child)
	appendTranscript(t, filepath.Join(root, "unrelated.jsonl"), messageLine("other", "opus", "end_turn", 3, 100000, 0, 0))
	if n := s.Flush(&tally, time.Second); n != 2 {
		t.Fatal(n)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	var output, cost int64
	for _, r := range rows {
		output += r.OutputTokens
		cost += r.CostMicroUSD
		if r.Price == nil || r.Price.Source != "table" {
			t.Fatal("missing pricing semantics")
		}
		if want := map[string]string{"haiku": "req_child", "opus": ""}[r.Model]; r.APIRequestID != want {
			t.Fatalf("%s api request id = %q, want %q", r.Model, r.APIRequestID, want)
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
	// Without a requestId the message id is only a dedup key, not something
	// the CLI's telemetry could ever name.
	if turns[0].ev.APIRequestID != "" {
		t.Fatalf("api request id from a message id: %q", turns[0].ev.APIRequestID)
	}
}

// Web chat rows keep the request id from the stream (or the transcript), so
// the CLI's per-request timing finds them whichever arrives first.
func TestClaudeChatRowsTakeTelemetryTiming(t *testing.T) {
	s, _ := newTestServer(t)
	tally := s.NewTally()
	withReq := func(line, req string) string {
		return strings.Replace(line, `{"type":"assistant",`, `{"type":"assistant","request_id":"`+req+`",`, 1)
	}
	if _, err := s.store.RecordRequestTimings([]store.RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_one", TTFTMs: 1500, DurationMS: 12700}}); err != nil {
		t.Fatal(err)
	}
	observe(&tally, "turn",
		withReq(messageLine("msg-one", "opus", "tool_use", 2, 1097, 100, 0), "req_one"),
		// A later snapshot without the id must not erase it.
		messageLine("msg-one", "opus", "tool_use", 2, 1097, 100, 0),
		withReq(messageLine("msg-two", "opus", "end_turn", 2, 33, 200, 0), "req_two"))
	if n := s.Flush(&tally, time.Second); n != 2 {
		t.Fatal(n)
	}
	if _, err := s.store.RecordRequestTimings([]store.RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_two", TTFTMs: 1990, DurationMS: 2009}}); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int64{}
	for _, e := range s.store.ListUsage(store.UsageFilter{User: "alice"}) {
		got[e.APIRequestID] = [2]int64{e.APITTFTMs, e.APIDurationMS}
	}
	if got["req_one"] != [2]int64{1500, 12700} || got["req_two"] != [2]int64{1990, 2009} {
		t.Fatalf("timings = %v", got)
	}
}

func TestClaudeRequestPricingDoesNotUseAggregateContext(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 1}, LongContextOver: 100, Long: &config.TokenRates{Input: 2}}}
	tally := s.NewTally()
	observe(&tally, "one", messageLine("a", "opus", "end_turn", 80, 0, 0, 0), messageLine("b", "opus", "end_turn", 80, 0, 0, 0))
	if n := s.Flush(&tally, time.Second); n != 2 {
		t.Fatal(n)
	}
	for _, row := range s.store.ListUsage(store.UsageFilter{User: "alice"}) {
		if row.InputTokens != 80 || row.CostMicroUSD != 80 {
			t.Fatalf("aggregate prompt applied long tier: %+v", row)
		}
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

func costStateLine(input, output, read, write int) string {
	return fmt.Sprintf(`{"type":"cost-state","sessionId":"provider-session","modelUsage":{"opus":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d,"costUSD":9}}}`, input, output, read, write)
}

// The prompt-suggestion request appears only in the cumulative cost-state the
// CLI writes as it exits; the growth since result is charged once per turn.
func TestClaudePromptSuggestionBilledFromCostStateGrowth(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 3}}}
	if _, err := s.store.Grant("alice", 1000000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace", "provider-session.jsonl")
	// A previous process's totals are history, never this turn's spend.
	appendTranscript(t, main, costStateLine(100, 1000, 5000, 20))
	result := `{"type":"result","session_id":"provider-session","modelUsage":{"opus":{"inputTokens":110,"outputTokens":1050,"cacheReadInputTokens":9000,"cacheCreationInputTokens":20}}}`
	run := func(turn string, suggest bool, final ...string) []store.UsageEvent {
		t.Helper()
		tally := s.NewChatTally(sess)
		if suggest {
			tally.MeterSuggestion()
		}
		observe(&tally, turn, `{"type":"system","session_id":"provider-session"}`, messageLine("msg-"+turn, "opus", "end_turn", 10, 50, 4000, 0), result)
		appendTranscript(t, main, append([]string{messageLine("msg-"+turn, "opus", "end_turn", 10, 50, 4000, 0)}, final...)...)
		s.Flush(&tally, time.Second)
		var out []store.UsageEvent
		for _, r := range s.store.ListUsage(store.UsageFilter{User: "alice"}) {
			if r.TurnID == turn {
				out = append(out, r)
			}
		}
		return out
	}
	rows := run("one", true, costStateLine(110, 1050, 9000, 20), costStateLine(610, 1160, 30000, 20))
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	var chat, suggestion store.UsageEvent
	for _, r := range rows {
		if r.Kind == store.UsageKindSuggestion {
			suggestion = r
		} else {
			chat = r
		}
	}
	if chat.Kind != store.UsageKindChat || chat.OutputTokens != 50 || chat.WallMS == 0 {
		t.Fatalf("chat row: %+v", chat)
	}
	if suggestion.InputTokens != 500 || suggestion.OutputTokens != 110 || suggestion.CacheReadTokens != 21000 || suggestion.CacheWriteTokens != 0 ||
		suggestion.CostMicroUSD != 500+220+2100 || suggestion.WallMS != 0 || suggestion.TTFTMs != 0 || suggestion.Price.Source != "table" {
		t.Fatalf("suggestion row: %+v", suggestion)
	}
	q, _ := s.store.GetQuota("alice")
	if q.BalanceMicroUSD != 1000000-chat.CostMicroUSD-suggestion.CostMicroUSD {
		t.Fatalf("balance %d", q.BalanceMicroUSD)
	}
	// Replaying the same turn's growth cannot charge it again.
	tally := s.NewChatTally(sess)
	tally.MeterSuggestion()
	observe(&tally, "one", `{"type":"system","session_id":"provider-session"}`, result)
	appendTranscript(t, main, costStateLine(610, 1160, 30000, 20))
	if n := s.Flush(&tally, time.Second); n != 0 {
		t.Fatal("suggestion charged twice", n)
	}
	// Not requested, no exit record, or shrinking totals: nothing is guessed.
	for _, c := range []struct {
		turn    string
		suggest bool
		final   []string
	}{
		{"plain", false, []string{costStateLine(999, 9999, 99999, 99)}},
		{"missing", true, nil},
		{"shrunk", true, []string{costStateLine(1, 1, 1, 1)}},
	} {
		if rows := run(c.turn, c.suggest, c.final...); len(rows) != 1 || rows[0].Kind != store.UsageKindChat {
			t.Fatalf("%s: %+v", c.turn, rows)
		}
	}
}

func costStateAt(start int64, input, output, read, write int) string {
	return fmt.Sprintf(`{"type":"cost-state","sessionId":"provider-session","startTime":%d,"modelUsage":{"opus":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d,"costUSD":9}}}`, start, input, output, read, write)
}
func resultTotals(input, output, read, write int) string {
	return fmt.Sprintf(`{"type":"result","subtype":"success","num_turns":0,"session_id":"provider-session","modelUsage":{"opus":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d}}}`, input, output, read, write)
}

// /compact's summary request reaches neither the stream nor the transcript's
// messages (Claude Code 2.1.296, checked against a local mock API); only the
// CLI's cumulative totals grow. That growth, less restored history and the
// messages metered one by one, is billed once as the turn's own spend.
func TestClaudeHiddenRequestsBilledFromCostState(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 3}}}
	main := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace", "provider-session.jsonl")
	rowsOf := func(turn string) []store.UsageEvent {
		var out []store.UsageEvent
		for _, r := range s.store.ListUsage(store.UsageFilter{User: "alice"}) {
			if r.TurnID == turn {
				out = append(out, r)
			}
		}
		return out
	}
	// Earlier process: one message, billed in its own turn, then its exit record.
	appendTranscript(t, main, `{"type":"last-prompt"}`)
	first := s.NewChatTally(sess)
	observe(&first, "first", `{"type":"system","session_id":"provider-session"}`, resultTotals(6021, 81, 2100, 147))
	appendTranscript(t, main, messageLine("old", "opus", "end_turn", 6021, 81, 2100, 147), costStateAt(1791988749804, 6021, 81, 2100, 147))
	if n := s.Flush(&first, time.Second); n != 1 {
		t.Fatal("first turn rows", n)
	}
	compact := func(turn string, start int64, lines ...string) {
		t.Helper()
		tally := s.NewChatTally(sess)
		observe(&tally, turn, `{"type":"system","subtype":"status","status":"compacting","session_id":"provider-session"}`,
			`{"type":"system","subtype":"compact_boundary","session_id":"provider-session","compact_metadata":{"trigger":"manual"}}`)
		// The preserved segment is replayed with its original ids: history, not this turn.
		observe(&tally, turn, lines...)
		observe(&tally, turn, resultTotals(7028, 98, 2800, 196))
		appendTranscript(t, main, `{"type":"user","isCompactSummary":true,"entrypoint":"sdk-cli","message":{"role":"user","content":"summary"}}`,
			costStateAt(start, 7028, 98, 2800, 196))
		s.Flush(&tally, 2*time.Second)
	}
	compact("compact", 1791988749804, messageLine("old", "opus", "end_turn", 6021, 81, 2100, 147))
	rows := rowsOf("compact")
	if len(rows) != 1 || rows[0].InputTokens != 1007 || rows[0].OutputTokens != 17 || rows[0].CacheReadTokens != 700 || rows[0].CacheWriteTokens != 49 ||
		rows[0].Kind != store.UsageKindChat || rows[0].Model != "opus" || rows[0].WallMS != 2000 || rows[0].CostMicroUSD != 1007+34+70+147 {
		t.Fatalf("compact rows: %+v", rows)
	}
	// Replaying the same turn cannot charge it again.
	tally := s.NewChatTally(sess)
	observe(&tally, "compact", `{"type":"system","session_id":"provider-session"}`, resultTotals(7028, 98, 2800, 196))
	appendTranscript(t, main, costStateAt(1791988749804, 7028, 98, 2800, 196))
	if n := s.Flush(&tally, time.Second); n != 0 {
		t.Fatal("hidden request charged twice", n)
	}

	// A turn whose messages account for all the growth adds nothing.
	tally = s.NewChatTally(sess)
	observe(&tally, "plain", `{"type":"system","session_id":"provider-session"}`,
		`{"type":"stream_event","session_id":"provider-session","event":{"type":"message_start","message":{"id":"m-plain","model":"opus","usage":{"input_tokens":10,"output_tokens":1}}}}`,
		`{"type":"stream_event","session_id":"provider-session","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}}`,
		resultTotals(7038, 103, 2800, 196))
	appendTranscript(t, main, costStateAt(1791988749804, 7038, 103, 2800, 196))
	if n := s.Flush(&tally, time.Second); n != 1 {
		t.Fatal("plain turn rows", n)
	}

	// A CLI that did not carry its totals over (a new startTime) spent all it shows.
	tally = s.NewChatTally(sess)
	observe(&tally, "fresh-totals", `{"type":"system","session_id":"provider-session"}`, resultTotals(500, 20, 0, 0))
	appendTranscript(t, main, costStateAt(1791999999999, 500, 20, 0, 0))
	s.Flush(&tally, time.Second)
	if rows := rowsOf("fresh-totals"); len(rows) != 1 || rows[0].InputTokens != 500 || rows[0].OutputTokens != 20 {
		t.Fatalf("unrestored totals: %+v", rows)
	}

	// Without the exit record a resumed turn's totals cannot be told from history.
	tally = s.NewChatTally(sess)
	observe(&tally, "no-exit", `{"type":"system","session_id":"provider-session"}`, resultTotals(9000, 900, 9000, 900))
	appendTranscript(t, main, `{"type":"last-prompt"}`)
	if n := s.Flush(&tally, time.Second); n != 0 {
		t.Fatal("guessed without exit record", n)
	}
}

// A new session has no history: its totals are all this process's own, so a
// request the stream never showed (WebFetch's digest) is billed without an
// earlier record. Totals that fall short, or messages under a model the
// totals do not name, leave the per-message rows alone.
func TestClaudeHiddenRequestsInNewSessionAndMismatches(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Output: 1}}}
	run := func(session, turn string, lines ...string) []store.UsageEvent {
		t.Helper()
		path := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace", session+".jsonl")
		tally := s.NewChatTally(sess)
		observe(&tally, turn, append([]string{`{"type":"system","session_id":"` + session + `"}`}, lines...)...)
		appendTranscript(t, path, `{"type":"last-prompt"}`)
		s.Flush(&tally, time.Second)
		var out []store.UsageEvent
		for _, r := range s.store.ListUsage(store.UsageFilter{User: "alice"}) {
			if r.TurnID == turn {
				out = append(out, r)
			}
		}
		return out
	}
	start := `{"type":"stream_event","event":{"type":"message_start","message":{"id":"%s","model":"%s","usage":{"input_tokens":10,"output_tokens":5}}}}`
	twoModels := `{"type":"result","modelUsage":{"opus":{"inputTokens":%d,"outputTokens":%d},"haiku":{"inputTokens":%d,"outputTokens":%d}}}`
	rows := run("new-a", "fetch", fmt.Sprintf(start, "m1", "opus-20260101"), fmt.Sprintf(twoModels, 10, 5, 300, 40))
	if len(rows) != 2 {
		t.Fatalf("new session rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Model == "haiku" && (r.InputTokens != 300 || r.OutputTokens != 40) || r.Model != "haiku" && r.OutputTokens != 5 {
			t.Fatalf("new session row: %+v", r)
		}
	}
	if rows := run("new-b", "short", fmt.Sprintf(start, "m2", "opus"), fmt.Sprintf(twoModels, 9, 5, 0, 0)); len(rows) != 1 {
		t.Fatalf("short totals: %+v", rows)
	}
	if rows := run("new-c", "renamed", fmt.Sprintf(start, "m3", "relay-name"), fmt.Sprintf(twoModels, 10, 5, 300, 40)); len(rows) != 1 {
		t.Fatalf("renamed model: %+v", rows)
	}
}

func TestClaudePromptCacheAndKeepaliveRows(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 10}}}
	if _, err := s.store.Grant("alice", 1000000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	tally := s.NewTally()
	if pc := tally.PromptCache(); pc != (PromptCache{}) {
		t.Fatal(pc)
	}
	base := chatBase("keep")
	base.Kind = store.UsageKindKeepalive
	for _, line := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":46000,"cache_creation_input_tokens":80,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":80}}}}}`,
		// A subagent's request is not the conversation the keepalive resumes.
		`{"type":"stream_event","parent_tool_use_id":"tool-a","event":{"type":"message_start","message":{"id":"child","model":"claude","usage":{"input_tokens":3,"output_tokens":1,"cache_creation_input_tokens":500,"cache_creation":{"ephemeral_5m_input_tokens":500}}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}}`,
	} {
		tally.Observe(base, []byte(line))
	}
	if pc := tally.PromptCache(); pc != (PromptCache{Prompt: 46082, Read: 46000, TTL: "1h"}) {
		t.Fatal(pc)
	}
	if n := s.Flush(&tally, time.Second); n != 2 {
		t.Fatal(n)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	spent := int64(0)
	for _, r := range rows {
		if r.Kind != store.UsageKindKeepalive {
			t.Fatalf("keepalive billed as %q", r.Kind)
		}
		spent += r.CostMicroUSD
	}
	if q, _ := s.store.GetQuota("alice"); spent == 0 || q.BalanceMicroUSD != 1000000-spent {
		t.Fatalf("keepalive not charged: spent %d, %+v", spent, q)
	}

	// The lifetime is the turn's: a later request that wrote nothing keeps it.
	turn := s.NewTally()
	observe(&turn, "turn",
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"t1","model":"claude","usage":{"input_tokens":2,"output_tokens":1,"cache_creation_input_tokens":900,"cache_creation":{"ephemeral_5m_input_tokens":900,"ephemeral_1h_input_tokens":0}}}}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"t2","model":"claude","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":902}}}}`)
	if pc := turn.PromptCache(); pc != (PromptCache{Prompt: 904, Read: 902, TTL: "5m"}) {
		t.Fatal(pc)
	}

	// /compact: the stream shows no request of its own, only the boundary,
	// after which the conversation is the summary. A sub-agent's does not count.
	compact := s.NewTally()
	observe(&compact, "compact",
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"c1","model":"claude","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":400000,"cache_creation_input_tokens":900,"cache_creation":{"ephemeral_1h_input_tokens":900}}}}}`,
		`{"type":"system","subtype":"compact_boundary","parent_tool_use_id":"tool-a","compact_metadata":{"trigger":"auto"}}`)
	if pc := compact.PromptCache(); pc.Compacted || pc.Prompt != 400902 {
		t.Fatal(pc)
	}
	observe(&compact, "compact", `{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"manual","pre_tokens":400902,"post_tokens":14000}}`)
	if pc := compact.PromptCache(); pc != (PromptCache{TTL: "1h", Compacted: true}) {
		t.Fatal(pc)
	}
	// Compacted mid-turn: the requests after it are the conversation now.
	observe(&compact, "compact",
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"c2","model":"claude","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":12000,"cache_creation_input_tokens":2000,"cache_creation":{"ephemeral_1h_input_tokens":2000}}}}}`)
	if pc := compact.PromptCache(); pc != (PromptCache{Prompt: 14002, Read: 12000, TTL: "1h"}) {
		t.Fatal(pc)
	}
}
