package usage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

// flakyStore fails the next writes, as a locked or full database would.
type flakyStore struct {
	*store.Store
	fail int
}

func (f *flakyStore) InsertUsage(evs ...store.UsageEvent) error {
	if f.fail > 0 {
		f.fail--
		return errors.New("database is locked")
	}
	return f.Store.InsertUsage(evs...)
}
func (f *flakyStore) InsertUsageMessages(evs ...store.UsageEvent) (int, error) {
	if f.fail > 0 {
		f.fail--
		return 0, errors.New("database is locked")
	}
	return f.Store.InsertUsageMessages(evs...)
}

func streamMessage(parent, id, model, request, stop string, input, output, read int) []string {
	p := ""
	if parent != "" {
		p = fmt.Sprintf(`"parent_tool_use_id":%q,`, parent)
	}
	return []string{
		fmt.Sprintf(`{"type":"stream_event",%s"event":{"type":"message_start","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":1,"cache_read_input_tokens":%d}}}}`, p, id, model, input, read),
		fmt.Sprintf(`{"type":"assistant",%s"request_id":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":1,"cache_read_input_tokens":%d}}}`, p, request, id, model, input, read),
		fmt.Sprintf(`{"type":"stream_event",%s"event":{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}}`, p, stop, output),
		fmt.Sprintf(`{"type":"stream_event",%s"event":{"type":"message_stop"}}`, p),
	}
}

// A web turn's Claude messages land in the usage log (and the balance) as each
// one's stream ends, not when the whole turn does. The end adds the turn's own
// latency and the provider name to the rows already written, and charges
// nothing twice.
func TestClaudeMessagesSettleAsTheyFinish(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = config.AgentClaude
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 1, Output: 10, CacheRead: 0.1}}}
	if _, err := s.store.Grant("alice", 1000000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	base := chatBase("turn")
	base.TTFTMs = 700
	tally := s.NewChatTally(sess)
	feed := func(lines ...string) {
		for _, line := range lines {
			tally.Observe(base, []byte(line))
		}
	}
	first := streamMessage("", "msg-one", "opus", "req_one", "tool_use", 2, 40, 1000)
	feed(first[:3]...)
	if rows := rowsOldestFirst(s); len(rows) != 0 {
		t.Fatalf("settled before the message ended: %+v", rows)
	}
	feed(first[3])
	rows := rowsOldestFirst(s)
	if len(rows) != 1 || rows[0].OutputTokens != 40 || rows[0].CacheReadTokens != 1000 || rows[0].APIRequestID != "req_one" ||
		rows[0].CostMicroUSD != 502 || rows[0].WallMS != 0 || rows[0].TTFTMs != 0 {
		t.Fatalf("first message not settled at once: %+v", rows)
	}
	if q, _ := s.store.GetQuota("alice"); q.BalanceMicroUSD != 1000000-502 {
		t.Fatalf("balance %d", q.BalanceMicroUSD)
	}
	feed(streamMessage("tool-a", "child", "haiku", "req_child", "end_turn", 3, 7, 0)...)
	feed(streamMessage("", "msg-two", "opus", "req_two", "end_turn", 2, 30, 1100)...)
	if rows = rowsOldestFirst(s); len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
	// A message whose stream never closed (interrupted) waits for the end.
	feed(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg-cut","model":"opus","usage":{"input_tokens":5,"output_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`)
	if rows = rowsOldestFirst(s); len(rows) != 3 {
		t.Fatalf("placeholder usage settled: %+v", rows)
	}
	feed(`{"type":"result","subtype":"success","duration_ms":2500,"modelUsage":{"opus":{"provider":"firstParty"},"haiku":{"provider":"firstParty"}}}`)
	if n := s.Flush(&tally, 3*time.Second); n != 4 {
		t.Fatal(n)
	}
	if n := s.Flush(&tally, 3*time.Second); n != 0 {
		t.Fatal("double flush", n)
	}
	rows = rowsOldestFirst(s)
	if len(rows) != 4 || rows[3].OutputTokens != 1 {
		t.Fatalf("%+v", rows)
	}
	var total int64
	for i, r := range rows {
		total += r.CostMicroUSD
		if r.Provider != "firstParty" {
			t.Fatalf("row %d provider %q", i, r.Provider)
		}
		timed := r.TTFTMs != 0 || r.DurationMS != 0 || r.WallMS != 0
		if timed != (i == 0) {
			t.Fatalf("turn latency must sit on the first request only: %+v", rows)
		}
	}
	if rows[0].TTFTMs != 700 || rows[0].DurationMS != 2500 || rows[0].WallMS != 3000 {
		t.Fatalf("turn latency %+v", rows[0])
	}
	if q, _ := s.store.GetQuota("alice"); q.BalanceMicroUSD != 1000000-total {
		t.Fatalf("balance %d, rows cost %d", q.BalanceMicroUSD, total)
	}
}

// A message whose write failed mid-turn is settled with the rest at the end.
func TestClaudeLiveSettlementFailureRetriedAtEnd(t *testing.T) {
	fixture, sess := newTestServer(t)
	sess.Agent = config.AgentClaude
	flaky := &flakyStore{Store: fixture.store, fail: 1}
	s := New(context.Background(), fixture.cfg, flaky)
	tally := s.NewChatTally(sess)
	for _, line := range append(streamMessage("", "msg-one", "opus", "req_one", "tool_use", 2, 40, 0),
		streamMessage("", "msg-two", "opus", "req_two", "end_turn", 2, 30, 0)...) {
		tally.Observe(chatBase("turn"), []byte(line))
	}
	if rows := rowsOldestFirst(fixture); len(rows) != 1 || rows[0].OutputTokens != 30 {
		t.Fatalf("%+v", rows)
	}
	if n := s.Flush(&tally, time.Second); n != 2 || tally.SettlementError() != nil {
		t.Fatal(n, tally.SettlementError())
	}
	got := map[string]store.UsageEvent{}
	for _, r := range rowsOldestFirst(fixture) {
		got[r.APIRequestID] = r
	}
	if len(got) != 2 || got["req_one"].OutputTokens != 40 || got["req_one"].WallMS != 1000 || got["req_two"].WallMS != 0 {
		t.Fatalf("%+v", got)
	}
}

func codexRequestLine(thread string, ts, input, cached, output int64) string {
	return fmt.Sprintf(`{"type":%q,"thread_id":%q,"request":{"ts":%d,"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}`,
		agent.RequestUsageType, thread, ts, input, cached, output)
}

// Codex app-server requests are settled as the CLI reports each one; the
// turn-end event lists them again and only what was not yet on record is added.
func TestCodexRequestsSettleAsTheyArrive(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = config.AgentCodex
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 1, CacheRead: 0.1, Output: 10}}}
	if _, err := s.store.Grant("alice", 1000000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	tally := s.NewChatTally(sess)
	tally.Observe(codexBase("one"), []byte(codexRequestLine("th-1", 1000, 150000, 100000, 10)))
	rows := rowsOldestFirst(s)
	if len(rows) != 1 || rows[0].InputTokens != 50000 || rows[0].CostMicroUSD != 60100 || rows[0].TTFTMs != 0 ||
		rows[0].APIRequestID != CodexRequestKey("th-1", 150000, 100000, 10) {
		t.Fatalf("first request not settled at once: %+v", rows)
	}
	// The second request is reported only by the turn-end event.
	end := `{"type":"turn.completed","thread_id":"th-1","usage":{"input_tokens":400000,"cached_input_tokens":300000,"output_tokens":30},"requests":[` +
		`{"ts":1000,"input_tokens":150000,"cached_input_tokens":100000,"output_tokens":10},` +
		`{"ts":2000,"input_tokens":250000,"cached_input_tokens":200000,"output_tokens":20}]}`
	tally.Observe(codexBase("one"), []byte(end))
	if n := s.Flush(&tally, 5*time.Second); n != 2 {
		t.Fatal(n)
	}
	rows = rowsOldestFirst(s)
	if len(rows) != 2 || rows[1].CostMicroUSD != 70200 {
		t.Fatalf("%+v", rows)
	}
	if rows[0].TTFTMs != 900 || rows[0].WallMS != 5000 || rows[1].TTFTMs != 0 || rows[1].WallMS != 0 {
		t.Fatalf("turn latency must sit on the first request only: %+v", rows)
	}
	if q, _ := s.store.GetQuota("alice"); q.BalanceMicroUSD != 1000000-60100-70200 {
		t.Fatalf("balance %d", q.BalanceMicroUSD)
	}
}

// A request whose write failed mid-turn is kept: the end settles it even when
// the CLI died before its turn-end event.
func TestCodexLiveSettlementFailureRetriedAtEnd(t *testing.T) {
	fixture, sess := newTestServer(t)
	sess.Agent = config.AgentCodex
	flaky := &flakyStore{Store: fixture.store, fail: 1}
	s := New(context.Background(), fixture.cfg, flaky)
	tally := s.NewChatTally(sess)
	tally.Observe(codexBase("one"), []byte(codexRequestLine("th-1", 1000, 150, 100, 10)))
	tally.Observe(codexBase("one"), []byte(codexRequestLine("th-1", 2000, 250, 200, 20)))
	if rows := rowsOldestFirst(fixture); len(rows) != 1 || rows[0].OutputTokens != 20 {
		t.Fatalf("%+v", rows)
	}
	if n := s.Flush(&tally, time.Second); n != 2 || tally.SettlementError() != nil {
		t.Fatal(n, tally.SettlementError())
	}
	if rows := rowsOldestFirst(fixture); len(rows) != 2 || rows[0].OutputTokens != 10 {
		t.Fatalf("%+v", rows)
	}
}

// Tallies that are not a web turn's (the image probe's) keep settling at the end.
func TestPlainTallyIgnoresLiveRequestRecords(t *testing.T) {
	s, _ := newTestServer(t)
	tally := s.NewTally()
	tally.Observe(codexBase("one"), []byte(codexRequestLine("th-1", 1000, 150, 100, 10)))
	if len(rowsOldestFirst(s)) != 0 {
		t.Fatal("settled outside a web turn")
	}
	if n := s.Flush(&tally, time.Second); n != 0 {
		t.Fatal(n)
	}
}
