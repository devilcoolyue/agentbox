package usage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// app-server 收尾事件的形状（agent/appserver_usage.go）：usage 是各次请求之和。
const codexMultiRequest = `{"type":"turn.completed","usage":{"input_tokens":400000,"cached_input_tokens":300000,"output_tokens":30},"requests":[` +
	`{"ts":1000,"input_tokens":150000,"cached_input_tokens":100000,"output_tokens":10},` +
	`{"ts":2000,"input_tokens":250000,"cached_input_tokens":200000,"output_tokens":20}]}`

func codexBase(turn string) store.UsageEvent {
	return store.UsageEvent{User: "alice", SessionID: "s1", ThreadID: "thread", TurnID: turn, Agent: "codex", Kind: store.UsageKindChat, TTFTMs: 900}
}

func rowsOldestFirst(s *fixtureService) []store.UsageEvent {
	return s.store.ListUsage(store.UsageFilter{User: "alice", Asc: true})
}

// Codex 回合逐次成行：每次请求自己查价判档、带自己的时间，回合级耗时只在第一行。
// 合计的提示词（40 万）远超阈值，但单次都没过线，都是标准档。
func TestCodexRequestsBecomeRows(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 1, CacheRead: 0.1, Output: 10}, LongContextOver: 272000, Long: &config.TokenRates{Input: 2, CacheRead: 0.2, Output: 20}}}
	tally := s.NewTally()
	tally.Observe(codexBase("one"), []byte(codexMultiRequest))
	if n := s.Flush(&tally, 5*time.Second); n != 2 {
		t.Fatal(n)
	}
	rows := rowsOldestFirst(s)
	if len(rows) != 2 {
		t.Fatalf("%+v", rows)
	}
	first, second := rows[0], rows[1]
	if first.TS.UnixMilli() != 1000 || first.InputTokens != 50000 || first.CacheReadTokens != 100000 || first.OutputTokens != 10 || first.CostMicroUSD != 60100 {
		t.Fatalf("first %+v", first)
	}
	if second.TS.UnixMilli() != 2000 || second.InputTokens != 50000 || second.CacheReadTokens != 200000 || second.CostMicroUSD != 70200 {
		t.Fatalf("second %+v", second)
	}
	if first.TTFTMs != 900 || first.WallMS != 5000 || second.TTFTMs != 0 || second.WallMS != 0 {
		t.Fatalf("turn latency must sit on the first request only: %+v / %+v", first, second)
	}
	if first.TurnID != "one" || second.TurnID != "one" || first.Price.PerRequest || MergedRequests(first) != 0 {
		t.Fatalf("rows %+v / %+v", first, second)
	}
	if first.APIRequestID != "" || second.APIRequestID != "" {
		t.Fatalf("keyed without a thread: %q / %q", first.APIRequestID, second.APIRequestID)
	}
}

// 收尾事件带线程 id 时，每次请求按「线程 + 这次的 token 数」记下对照键，等
// Codex 遥测的实测计时来对（输入含缓存命中，与遥测同口径）。
func TestCodexRequestsKeyedByThread(t *testing.T) {
	s, _ := newTestServer(t)
	tally := s.NewTally()
	tally.Observe(codexBase("one"), []byte(strings.Replace(codexMultiRequest, `"requests"`, `"thread_id":"01a1581b-c0de","requests"`, 1)))
	s.Flush(&tally, time.Second)
	rows := rowsOldestFirst(s)
	if len(rows) != 2 || rows[0].APIRequestID != "codex:01a1581b-c0de:150000:100000:10" || rows[1].APIRequestID != CodexRequestKey("01a1581b-c0de", 250000, 200000, 20) {
		t.Fatalf("%+v", rows)
	}
	if CodexRequestKey("", 1, 0, 1) != "" || CodexRequestKey("t", -1, 0, 1) != "" {
		t.Fatal("bad key accepted")
	}
}

// 单次请求过线时只有那一行走长档。
func TestCodexLongRequestPricedAlone(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 1}, LongContextOver: 200000, Long: &config.TokenRates{Input: 2}}}
	tally := s.NewTally()
	tally.Observe(codexBase("one"), []byte(codexMultiRequest))
	s.Flush(&tally, time.Second)
	rows := rowsOldestFirst(s)
	if rows[0].CostMicroUSD != 50000 || rows[1].CostMicroUSD != 100000 {
		t.Fatalf("%d / %d", rows[0].CostMicroUSD, rows[1].CostMicroUSD)
	}
}

// 中断/失败的 app-server 回合带着已发生请求的用量，照样入账；exec 回退路径
// 只有合计的收尾事件仍是一行。
func TestCodexFailedTurnAndExecFallback(t *testing.T) {
	s, _ := newTestServer(t)
	tally := s.NewTally()
	tally.Observe(codexBase("one"), []byte(`{"type":"turn.failed","status":"interrupted","error":{"message":"x"},"usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2},"requests":[{"ts":1,"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}]}`))
	tally.Observe(codexBase("one"), []byte(`{"type":"turn.failed","error":{"message":"exec 回退路径不带用量"}}`))
	if n := s.Flush(&tally, time.Second); n != 1 {
		t.Fatal(n)
	}
	if row := rowsOldestFirst(s)[0]; row.InputTokens != 6 || row.CacheReadTokens != 4 {
		t.Fatalf("%+v", row)
	}
	exec := s.NewTally()
	exec.Observe(codexBase("two"), []byte(`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":5}}`))
	if n := s.Flush(&exec, time.Second); n != 1 {
		t.Fatal(n)
	}
}

// Claude 网页对话：每条消息一行，按出现时间排；时间取流里首次看到与 transcript
// 时间中较早者；回合级耗时只在第一条对话请求上。
func TestClaudeMessagesBecomeRows(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 1, Output: 10, CacheRead: 0.1, CacheWrite: 2}}}
	main := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace", "provider-session.jsonl")
	appendTranscript(t, main)
	tally := s.NewChatTally(sess)
	before := time.Now()
	base := chatBase("one")
	base.TTFTMs = 1500
	for _, line := range []string{`{"type":"system","session_id":"provider-session"}`, messageLine("b", "opus", "", 2, 1, 1000, 0)} {
		tally.Observe(base, []byte(line))
	}
	// z 在 transcript 里的时间早于流里看到 b 的时刻；b 的 transcript 时间更晚，
	// 保留流里那份。行按时间排，不按消息 ID 排。
	appendTranscript(t, main,
		`{"type":"assistant","entrypoint":"sdk-cli","timestamp":"2026-10-14T02:19:50.123Z","message":{"id":"z","model":"opus","stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5,"cache_read_input_tokens":900,"cache_creation_input_tokens":10}}}`,
		`{"type":"assistant","entrypoint":"sdk-cli","timestamp":"2999-01-01T00:00:00Z","message":{"id":"b","model":"opus","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":7,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0}}}`)
	if n := s.Flush(&tally, 4*time.Second); n != 2 {
		t.Fatal(n)
	}
	rows := rowsOldestFirst(s)
	z, b := rows[0], rows[1]
	if !z.TS.Equal(time.Date(2026, 10, 14, 2, 19, 50, 123e6, time.UTC)) || z.CacheWriteTokens != 10 || z.CostMicroUSD != 163 || z.ReqID != "" {
		t.Fatalf("z %+v", z)
	}
	if b.TS.Before(before.Truncate(time.Millisecond)) || b.TS.After(time.Now()) || b.OutputTokens != 7 {
		t.Fatalf("b %+v", b)
	}
	if z.TTFTMs != 1500 || z.WallMS != 4000 || b.TTFTMs != 0 || b.WallMS != 0 {
		t.Fatalf("turn latency %+v / %+v", z, b)
	}
	if z.Price.PerRequest || MergedRequests(z) != 0 {
		t.Fatal("single-request rows are priced as such")
	}
}

// 升级前按回合合并的旧行保留原样，只报合并了几次。
func TestMergedRequestsLegacyRows(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"type":"result","usage":{}}`, `{"type":"codex.request","input_tokens":1}`, `{"id":"m","model":"opus","usage":{"input_tokens":1}}`} {
		if n := MergedRequests(store.UsageEvent{Raw: raw}); n != 0 {
			t.Fatalf("%q: %d", raw, n)
		}
	}
	if n := MergedRequests(store.UsageEvent{Raw: `{"type":"claude.messages","messages":[{"id":"a"},{"id":"b"},{"id":"c"}]}`}); n != 3 {
		t.Fatal(n)
	}
}
