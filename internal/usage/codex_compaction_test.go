package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func codexCompactionFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-terminal-0.163.0-compaction.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func tokensOf(r termTurn) [3]int64 {
	return [3]int64{r.ev.InputTokens + r.ev.CacheReadTokens, r.ev.CacheReadTokens, r.ev.OutputTokens}
}

// ChatGPT 登录的远程压缩不进累计：从 compacted 补一行，带它自己的回合与对照键。
func TestCodexTerminalRecordsRemoteCompaction(t *testing.T) {
	rows, err := parseCodexTerminal(strings.NewReader(codexCompactionFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := [][3]int64{{14068, 12160, 44}, {14176, 13952, 34}, {14230, 12160, 255}, {3000, 2000, 40}}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for i, r := range rows {
		if tokensOf(r) != want[i] {
			t.Fatalf("row %d tokens %v, want %v", i, tokensOf(r), want[i])
		}
	}
	compaction := rows[2]
	if compaction.ev.APIRequestID != CodexRequestKey("compact-thread", 14230, 12160, 255) ||
		compaction.ev.TurnID == rows[1].ev.TurnID || compaction.ev.TurnID == rows[3].ev.TurnID ||
		compaction.reqID == rows[1].reqID || compaction.model != "gpt-6.1-sol" || compaction.ts.IsZero() {
		t.Fatalf("compaction row %+v", compaction)
	}

	// 以后的版本若把压缩算进累计，下一次请求只认它自己的 last。
	later := strings.Replace(codexCompactionFixture(t),
		`"total_token_usage":{"input_tokens":31244,"cached_input_tokens":28112,"cache_write_input_tokens":0,"output_tokens":118,"reasoning_output_tokens":0,"total_tokens":31362}`,
		`"total_token_usage":{"input_tokens":45474,"cached_input_tokens":40272,"cache_write_input_tokens":0,"output_tokens":373,"reasoning_output_tokens":0,"total_tokens":45847}`, 1)
	if later == codexCompactionFixture(t) {
		t.Fatal("fixture no longer has the token_count this case rewrites")
	}
	rows, _ = parseCodexTerminal(strings.NewReader(later))
	if len(rows) != 4 || tokensOf(rows[3]) != [3]int64{3000, 2000, 40} {
		t.Fatalf("compaction counted twice: %+v", rows)
	}
	// 或者单独写一条只多出压缩用量的 token_count：那是已经补过的那次。
	alone := strings.Replace(codexCompactionFixture(t),
		`{"timestamp":"2026-10-20T07:03:40.414Z"`,
		`{"timestamp":"2026-10-20T07:03:40.410Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":42474,"cached_input_tokens":38272,"output_tokens":333},"last_token_usage":{"input_tokens":14230,"cached_input_tokens":12160,"output_tokens":255}}}}
{"timestamp":"2026-10-20T07:03:40.414Z"`, 1)
	if alone == codexCompactionFixture(t) {
		t.Fatal("fixture no longer has the line this case inserts before")
	}
	rows, _ = parseCodexTerminal(strings.NewReader(alone))
	if len(rows) != 4 {
		t.Fatalf("compaction counted twice: %+v", rows)
	}
}

// API Key / 中转的本地压缩是一次普通请求，已经在累计里：compacted 不再补。
func TestCodexTerminalSkipsLocalCompaction(t *testing.T) {
	raw := `{"timestamp":"2026-10-20T08:00:00.000Z","type":"session_meta","payload":{"id":"local-thread","originator":"codex-tui","source":"vscode"}}
{"timestamp":"2026-10-20T08:00:00.100Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}
{"timestamp":"2026-10-20T08:00:00.200Z","type":"turn_context","payload":{"turn_id":"t1","model":"gpt-5.5"}}
{"timestamp":"2026-10-20T08:00:01.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1400,"cached_input_tokens":500,"output_tokens":54},"last_token_usage":{"input_tokens":1400,"cached_input_tokens":500,"output_tokens":54}}}}
{"timestamp":"2026-10-20T08:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":2900,"cached_input_tokens":1000,"output_tokens":109},"last_token_usage":{"input_tokens":1500,"cached_input_tokens":500,"output_tokens":55}}}}
{"timestamp":"2026-10-20T08:00:02.100Z","type":"compacted","payload":{"latest_token_usage_record":{"turn_id":"t1","response_id":"resp_5","usage":{"input_tokens":1500,"cached_input_tokens":500,"output_tokens":55}}}}
{"timestamp":"2026-10-20T08:00:02.200Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":2900,"cached_input_tokens":1000,"output_tokens":109},"last_token_usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}}}}
{"timestamp":"2026-10-20T08:00:03.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":4500,"cached_input_tokens":1500,"output_tokens":165},"last_token_usage":{"input_tokens":1600,"cached_input_tokens":500,"output_tokens":56}}}}
`
	rows, err := parseCodexTerminal(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || tokensOf(rows[1]) != [3]int64{1500, 500, 55} || tokensOf(rows[2]) != [3]int64{1600, 500, 56} {
		t.Fatalf("rows %+v", rows)
	}
}

func writeCodexRollout(t *testing.T, s *fixtureService, sess store.Session, thread, body string) {
	t.Helper()
	path := filepath.Join(s.homeDir(sess), ".codex", "sessions", "2026", "10", "20", "rollout-2026-10-20T07-02-39-"+thread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const compactionThread = "01a1581e-0000-7000-8000-00000000c0de"

// 网页对话：回合里压缩过时结算读会话记录，远程压缩补一行（同回合、按对照键等
// 遥测计时），已在 tokenUsage 里的本地压缩与别的回合的不补。
func TestCodexWebCompactionRows(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = config.AgentCodex
	s.cfg.Pricing = map[string]config.ModelPrice{"gpt-6.1-sol": {TokenRates: config.TokenRates{Input: 1, CacheRead: 0.1, Output: 10}}}
	writeCodexRollout(t, s, sess, compactionThread,
		`{"timestamp":"2026-10-20T07:03:30.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"compacted"}]}}
{"timestamp":"2026-10-20T07:03:40.395Z","type":"compacted","payload":{"latest_token_usage_record":{"turn_id":"U2","response_id":"resp_remote","usage":{"input_tokens":14230,"cached_input_tokens":12160,"output_tokens":255}}}}
{"timestamp":"2026-10-20T07:03:41.000Z","type":"compacted","payload":{"latest_token_usage_record":{"turn_id":"U2","response_id":"resp_local","usage":{"input_tokens":1500,"cached_input_tokens":500,"output_tokens":55}}}}
{"timestamp":"2026-10-20T07:00:00.000Z","type":"compacted","payload":{"latest_token_usage_record":{"turn_id":"U1","response_id":"resp_old","usage":{"input_tokens":9000,"cached_input_tokens":0,"output_tokens":90}}}}
`)
	base := store.UsageEvent{User: sess.User, SessionID: sess.ID, ThreadID: "web-thread", TurnID: "turn-a", Agent: config.AgentCodex, Model: "gpt-6.1-sol", Kind: store.UsageKindChat}
	tally := s.NewChatTally(sess)
	tally.Observe(base, []byte(`{"type":"turn.completed","usage":{"input_tokens":3100,"cached_input_tokens":1000,"output_tokens":95},"requests":[`+
		`{"ts":1792479800000,"input_tokens":1500,"cached_input_tokens":500,"output_tokens":55},{"ts":1792479801000,"input_tokens":1600,"cached_input_tokens":500,"output_tokens":40}],`+
		`"thread_id":"`+compactionThread+`","turn_id":"U2","compactions":2}`))
	if n := s.Flush(&tally, 3*time.Second); n != 3 {
		t.Fatalf("flushed %d", n)
	}
	rows := rowsOldestFirst(s)
	var compaction *store.UsageEvent
	for i := range rows {
		if strings.Contains(rows[i].Raw, `"codex.compaction"`) {
			compaction = &rows[i]
		}
	}
	if compaction == nil || len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	if compaction.InputTokens != 14230-12160 || compaction.CacheReadTokens != 12160 || compaction.OutputTokens != 255 ||
		compaction.TurnID != "turn-a" || compaction.Kind != store.UsageKindChat || compaction.WallMS != 0 ||
		compaction.APIRequestID != CodexRequestKey(compactionThread, 14230, 12160, 255) || compaction.CostMicroUSD != 2070+1216+2550 ||
		!SingleRequest(*compaction) || compaction.TS.Format(time.RFC3339) != "2026-10-20T07:03:40Z" {
		t.Fatalf("compaction row %+v", *compaction)
	}

	// 压缩完就被中断、没有别的请求的回合同样要补；找不到会话记录不报错。
	tally = s.NewChatTally(sess)
	tally.Observe(base, []byte(`{"type":"turn.failed","status":"interrupted","thread_id":"`+compactionThread+`","turn_id":"U1","compactions":1}`))
	if n := s.Flush(&tally, time.Second); n != 1 {
		t.Fatalf("interrupted turn flushed %d", n)
	}
	tally = s.NewChatTally(sess)
	tally.Observe(base, []byte(`{"type":"turn.failed","status":"interrupted","thread_id":"01a1581e-0000-7000-8000-000000000000","turn_id":"U9","compactions":1}`))
	if n := s.Flush(&tally, time.Second); n != 0 {
		t.Fatalf("missing rollout flushed %d", n)
	}
	// 没压缩过的回合不读会话记录（Claude 空间也不会有这份状态）。
	if claude := s.NewChatTally(store.Session{ID: "c", User: "alice", Agent: config.AgentClaude}); claude.codexHome != "" {
		t.Fatal("Claude tally reads Codex rollouts")
	}
}
