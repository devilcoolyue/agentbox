package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func codexFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-terminal-0.145.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func codexTimingFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-terminal-0.163.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCodexTerminalDeduplicatesAndGroupsModels(t *testing.T) {
	rows, err := parseCodexTerminal(strings.NewReader(codexFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 request rows, got %+v", rows)
	}
	// One row per request; repeated token_count and reasoning are not double counted.
	if rows[0].ev.InputTokens != 60 || rows[0].ev.CacheReadTokens != 40 || rows[0].ev.OutputTokens != 10 ||
		rows[1].ev.InputTokens != 30 || rows[1].ev.CacheReadTokens != 20 || rows[1].ev.OutputTokens != 5 {
		t.Fatalf("repeated usage/reasoning double counted %+v / %+v", rows[0], rows[1])
	}
	// The first request keeps the old merged row's key; later ones append a sequence.
	if rows[1].reqID != rows[0].reqID+":1" || rows[1].ev.TurnID != rows[0].ev.TurnID || strings.Contains(rows[0].reqID[len("codex-terminal:"):], ":") {
		t.Fatalf("keys %q / %q", rows[0].reqID, rows[1].reqID)
	}
	if rows[2].ev.InputTokens != 20 || rows[2].ev.OutputTokens != 2 || rows[2].ev.TurnID != rows[0].ev.TurnID {
		t.Fatal(rows[2])
	}
	if rows[3].ev.InputTokens != 10 || rows[3].ev.OutputTokens != 3 || rows[3].ev.TurnID == rows[0].ev.TurnID {
		t.Fatal(rows[3])
	}
	// Each request is keyed by session and its own tokens (input including
	// cached) for the CLI's telemetry.
	for i, row := range rows {
		want := CodexRequestKey("fixture-thread", row.ev.InputTokens+row.ev.CacheReadTokens, row.ev.CacheReadTokens, row.ev.OutputTokens)
		if row.ev.APIRequestID != want || want == "" {
			t.Fatalf("row %d key %q, want %q", i, row.ev.APIRequestID, want)
		}
	}
	for i, want := range []string{"high", "high", "medium", ""} {
		if rows[i].ev.Effort != want || (want != "" && rows[i].ev.EffortControl != "effort") {
			t.Fatalf("row %d effort = %+v, want %q", i, rows[i].ev, want)
		}
	}
	for _, origin := range []string{"codex_exec", "codex-app-server", "unknown"} {
		raw := strings.ReplaceAll(codexFixture(t), "codex-tui", origin)
		rows, err := parseCodexTerminal(strings.NewReader(raw))
		if err != nil || len(rows) != 0 {
			t.Fatalf("nonterminal usage accepted: %s", origin)
		}
	}
}

// codex-tui 从 0.160 起跑在 app-server 上，rollout 的 source 写 "vscode"（实测
// 0.160.1 / 0.161.0）；它仍是终端里手敲的会话。agentbox 自己的网页对话同样写
// "vscode"，但 originator 是 "agentbox"；exec、subagent 依旧排除。
func TestCodexTerminalSourceAcrossVersions(t *testing.T) {
	for _, c := range []struct {
		originator, source string
		want               bool
	}{
		{"codex-tui", `"cli"`, true},
		{"codex-tui", `"vscode"`, true},
		{"agentbox", `"vscode"`, false},
		{"codex_exec", `"exec"`, false},
		{"codex-tui", `"exec"`, false},
		{"codex-tui", `{"subagent":{"thread_spawn":{"parent_thread_id":"p"}}}`, false},
	} {
		raw := strings.Replace(codexFixture(t), `"originator": "codex-tui", "source": "cli"`, `"originator": "`+c.originator+`", "source": `+c.source, 1)
		rows, err := parseCodexTerminal(strings.NewReader(raw))
		if err != nil || (len(rows) == 4) != c.want || (!c.want && len(rows) != 0) {
			t.Fatalf("%s/%s: %d rows, %v", c.originator, c.source, len(rows), err)
		}
	}
}
func TestCodexTerminalScanUpdatesWithoutChargingAndKeepsSnapshot(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "codex"
	sess.AccountID = "acct"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 2, Output: 3}}}
	if _, err := s.store.Grant(sess.User, 1000, "test-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.homeDir(sess), ".codex", "sessions", "2026", "09", "23", "rollout-fixture.jsonl")
	os.MkdirAll(filepath.Dir(path), 0755)
	fixture := codexFixture(t)
	lines := strings.Split(fixture, "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[:5], "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Scan()
	rows := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	s.cfg.Pricing["codex"] = config.ModelPrice{TokenRates: config.TokenRates{Input: 99, Output: 99}}
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	s.Scan()
	s.Scan()
	rows = s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 4 {
		t.Fatal(rows)
	}
	var fixtureA int64
	for _, r := range rows {
		// The turn's second request first appears after the price edit and
		// still inherits the turn's original snapshot.
		if r.Model == "gpt-fixture-a" && r.Price.Standard.Input != 2 {
			t.Fatalf("growth repriced %+v", r)
		}
		if r.Model == "gpt-fixture-a" {
			fixtureA += r.CostMicroUSD
		}
		if r.Model == "gpt-fixture-a" && (r.Effort != "high" || r.EffortControl != "effort") {
			t.Fatalf("transcript effort not persisted: %+v", r)
		}
	}
	if fixtureA != 225 {
		t.Fatalf("fixture-a cost %d", fixtureA)
	}
	q, _ := s.store.GetQuota(sess.User)
	if q.BalanceMicroUSD != 1000 {
		t.Fatal("terminal debited balance")
	}
}

func TestCodexTerminalEffortStaysUnknownWhenMissingOrMixed(t *testing.T) {
	fixture := codexFixture(t)
	for name, raw := range map[string]string{
		"missing":     strings.ReplaceAll(fixture, `, "effort": "high"`, ""),
		"null":        strings.ReplaceAll(fixture, `"effort": "high"`, `"effort": null`),
		"unsupported": strings.ReplaceAll(fixture, `"effort": "high"`, `"effort": {"future": true}`),
		"mixed":       strings.Replace(fixture, `{"timestamp": "2026-09-23T10:00:05Z"`, `{"type":"turn_context","payload":{"turn_id":"turn-one","model":"gpt-fixture-a","effort":"low"}}`+"\n"+`{"timestamp": "2026-09-23T10:00:05Z"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := parseCodexTerminal(strings.NewReader(raw))
			if err != nil || len(rows) != 4 || rows[0].ev.InputTokens != 60 || rows[1].ev.OutputTokens != 5 {
				t.Fatalf("effort changed usage: %+v, %v", rows, err)
			}
			// Each request carries the effort recorded before it; nothing is invented.
			want := map[string][2]string{"mixed": {"high", "low"}}[name]
			for i, r := range rows[:2] {
				if r.ev.Effort != want[i] || (want[i] == "" && r.ev.EffortControl != "") {
					t.Fatalf("request %d effort = %+v, want %q", i, r.ev, want[i])
				}
			}
		})
	}
}

func TestCodexTerminalEffortContextBeforeStart(t *testing.T) {
	lines := strings.Split(codexFixture(t), "\n")
	lines[1], lines[2] = lines[2], lines[1]
	rows, err := parseCodexTerminal(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil || len(rows) != 4 || rows[0].ev.Effort != "high" {
		t.Fatalf("same-turn start erased recorded context: %+v, %v", rows, err)
	}
}
func TestCodexCounterResetAndPartialTail(t *testing.T) {
	raw := codexFixture(t) + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"reset"}}
{"type":"turn_context","payload":{"turn_id":"reset","model":"gpt-fixture-b"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"output_tokens":0},"last_token_usage":{"input_tokens":180,"output_tokens":20}}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5,"output_tokens":2},"last_token_usage":{"input_tokens":5,"output_tokens":2}}}}
{"type":"event_msg","payload":`
	rows, err := parseCodexTerminal(strings.NewReader(raw))
	if err != nil || len(rows) != 5 {
		t.Fatalf("partial/reset: %v %+v", err, rows)
	}
	if rows[4].ev.InputTokens != 5 || rows[4].ev.OutputTokens != 2 {
		t.Fatal("reset duplicated previous usage")
	}
}

func TestCodexCopiesInDifferentWorkspacesDoNotOverwrite(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "codex"
	fixture := codexFixture(t)
	for _, id := range []string{"one", "two"} {
		sess.ID = id
		if err := s.store.Put(sess); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(s.homeDir(sess), ".codex", "sessions", "2026", "09", "23", "rollout-fixture.jsonl")
		os.MkdirAll(filepath.Dir(path), 0755)
		if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.Scan()
	rows := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 8 {
		t.Fatalf("workspace identity lost: %d rows", len(rows))
	}
}

// 0.163 的 rollout 在响应结束时写 token_usage_record，item_completed 给思考和回复
// 记了开始生成的时刻（started_at_ms），耗时与首字都由它们定。样本照实际事件顺序
// 合成：developer/user 消息与 turn_context 是起点，工具结果晚于用量记录写出，
// 限额刷新时重发的 token_count 不是一次请求。
func TestCodexTerminalTiming(t *testing.T) {
	rows, err := parseCodexTerminal(strings.NewReader(codexTimingFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int64{
		{3810, 3790}, // 用户消息 00.040 → 用量记录 03.850；回复从 03.830 开始
		{2680, 1480}, // 10.020 → 12.700（工具结果 12.800 晚于响应结束）
		{3797, 2797}, // 上一次的 token_count 12.803 → 16.600；思考 15.600
		{5398, 3448}, // 16.652 → 22.050；17.000 的重发快照不挪起点
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i, w := range want {
		if rows[i].ev.WallMS != w[0] || rows[i].ev.TTFTMs != w[1] {
			t.Errorf("row %d wall/ttft = %d/%d, want %v", i, rows[i].ev.WallMS, rows[i].ev.TTFTMs, w)
		}
	}
}

// 没有 token_usage_record 的旧布局（照 cc-switch 的回归样本）：
//   - token_count 等工具跑完才写：结束取最后一个输出项；
//   - 2025 年的布局把输出项补写在自己那次的 token_count 之后：不算下一次的起点，
//     用户隔了很久才发下一条时耗时不能把空闲算进去。
func TestCodexTerminalTimingWithoutUsageRecord(t *testing.T) {
	head := `{"timestamp":"%[1]s:00.000Z","type":"session_meta","payload":{"id":"th","originator":"codex-tui","source":"cli"}}
{"timestamp":"%[1]s:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}
`
	tc := func(ts string, in, out, lastIn, lastOut int) string {
		return fmt.Sprintf(`{"timestamp":"%s","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":%d},"last_token_usage":{"input_tokens":%d,"output_tokens":%d}}}}`+"\n", ts, in, out, lastIn, lastOut)
	}
	item := func(ts, payload string) string {
		return fmt.Sprintf(`{"timestamp":"%s","type":"response_item","payload":%s}`+"\n", ts, payload)
	}
	const user, reasoning, assistant = `{"type":"message","role":"user"}`, `{"type":"reasoning"}`, `{"type":"message","role":"assistant"}`
	const call, output = `{"type":"custom_tool_call","call_id":"c"}`, `{"type":"custom_tool_call_output","call_id":"c"}`
	for name, c := range map[string]struct {
		raw  string
		want []int64
	}{
		"token_count after tools": {fmt.Sprintf(head, "2026-08-03T03:00") +
			item("2026-08-03T03:00:01.000Z", user) +
			item("2026-08-03T03:00:06.000Z", call) +
			item("2026-08-03T03:00:16.000Z", output) +
			tc("2026-08-03T03:00:16.001Z", 100, 600, 100, 600) +
			tc("2026-08-03T03:00:17.000Z", 100, 600, 100, 600) +
			item("2026-08-03T03:00:20.000Z", reasoning) +
			item("2026-08-03T03:00:22.000Z", assistant) +
			tc("2026-08-03T03:00:22.100Z", 300, 1300, 200, 700),
			[]int64{5000, 6099}},
		"items after token_count": {fmt.Sprintf(head, "2025-10-30T08:00") +
			item("2025-10-30T08:00:01.000Z", user) +
			tc("2025-10-30T08:00:08.000Z", 100, 600, 100, 600) +
			item("2025-10-30T08:00:08.000Z", reasoning) +
			item("2025-10-30T08:00:08.001Z", call) +
			item("2025-10-30T08:00:08.001Z", output) +
			`{"timestamp":"2025-10-30T08:00:08.002Z","type":"turn_context","payload":{"turn_id":"t","model":"gpt-5"}}` + "\n" +
			item("2025-10-30T08:01:40.000Z", user) +
			tc("2025-10-30T08:01:50.000Z", 300, 1300, 200, 700),
			[]int64{7000, 10000}},
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := parseCodexTerminal(strings.NewReader(c.raw))
			if err != nil || len(rows) != len(c.want) {
				t.Fatalf("rows = %+v, %v", rows, err)
			}
			for i, w := range c.want {
				if rows[i].ev.WallMS != w || rows[i].ev.TTFTMs != 0 {
					t.Errorf("row %d wall/ttft = %d/%d, want %d/0", i, rows[i].ev.WallMS, rows[i].ev.TTFTMs, w)
				}
			}
		})
	}
}
