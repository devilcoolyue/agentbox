package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// 一份贴着真实 transcript 形状的样本：
//   - 两条 sdk-cli 记录：我们自己发起的对话，已经记过账，必须一条都不带进来
//   - 终端第一问：一次工具调用 + 一次收尾，其中工具调用那次在文件里重复出现
//     （流式中途写一遍、收尾再写一遍），只能算一次
//   - 终端第二问：换了模型，另起一个回合
//   - 一条 <synthetic> 空回复：CLI 本地合成的，没花钱
const termFixture = `
{"type":"user","entrypoint":"cli","uuid":"u1","timestamp":"2026-08-11T10:00:00.000Z","message":{"role":"user"}}
{"type":"assistant","entrypoint":"cli","uuid":"a1","requestId":"req_A","timestamp":"2026-08-11T10:00:02.000Z","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":4,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}}
{"type":"assistant","entrypoint":"cli","uuid":"a1b","requestId":"req_A","timestamp":"2026-08-11T10:00:03.000Z","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":4,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}}
{"type":"assistant","entrypoint":"cli","uuid":"a2","requestId":"req_B","timestamp":"2026-08-11T10:00:05.000Z","message":{"id":"msg_2","model":"claude-sonnet-5","usage":{"input_tokens":2,"output_tokens":50,"cache_read_input_tokens":1200,"cache_creation_input_tokens":0}}}
{"type":"assistant","entrypoint":"cli","uuid":"a3","requestId":"req_C","timestamp":"2026-08-11T10:00:06.000Z","message":{"id":"msg_3","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"user","entrypoint":"sdk-cli","uuid":"u9","timestamp":"2026-08-11T10:01:00.000Z","message":{"role":"user"}}
{"type":"assistant","entrypoint":"sdk-cli","uuid":"a9","requestId":"req_Z","timestamp":"2026-08-11T10:01:02.000Z","message":{"id":"msg_9","model":"claude-opus-5","usage":{"input_tokens":9,"output_tokens":900,"cache_read_input_tokens":9000,"cache_creation_input_tokens":900}}}
{"type":"user","entrypoint":"cli","uuid":"u2","timestamp":"2026-08-11T10:02:00.000Z","message":{"role":"user"}}
{"type":"assistant","entrypoint":"cli","uuid":"a4","requestId":"req_D","timestamp":"2026-08-11T10:02:04.000Z","message":{"id":"msg_4","model":"claude-haiku-4-5","usage":{"input_tokens":7,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
`

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(path, []byte(termFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeTerminalEffortUsesRecordedEffectiveValue(t *testing.T) {
	fixture := strings.ReplaceAll(termFixture, `"type":"assistant",`, `"type":"assistant","effort":"xhigh","perTurnEffort":"low",`)
	for name, raw := range map[string]string{
		"recorded":       fixture,
		"missing":        strings.ReplaceAll(fixture, `"effort":"xhigh",`, ""),
		"unknown shape":  strings.ReplaceAll(fixture, `"effort":"xhigh"`, `"effort":{"future":true}`),
		"mixed requests": strings.Replace(fixture, `"effort":"xhigh","perTurnEffort":"low","entrypoint":"cli","uuid":"a2"`, `"effort":"high","perTurnEffort":"low","entrypoint":"cli","uuid":"a2"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := parseTerminalReader(strings.NewReader(raw))
			if err != nil || len(rows) != 3 || rows[0].ev.OutputTokens != 100 || rows[1].ev.OutputTokens != 50 {
				t.Fatalf("effort changed usage: %+v, %v", rows, err)
			}
			// 一次请求一行，各自带自己记录的强度；混用不再互相抹掉。
			want := map[string][2]string{"recorded": {"xhigh", "xhigh"}, "mixed requests": {"xhigh", "high"}}[name]
			for i, r := range rows[:2] {
				if r.ev.Effort != want[i] || (want[i] != "" && r.ev.EffortControl != "effort") || (want[i] == "" && r.ev.EffortControl != "") {
					t.Fatalf("request %d effort = %+v, want %q", i, r.ev, want[i])
				}
			}
		})
	}
}

// Fixtures are service-owned; production opens transcripts through safefs.
func parseTerminalTranscript(path string) ([]termTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseTerminalReader(f)
}

func TestParseTerminalTranscript(t *testing.T) {
	turns, err := parseTerminalTranscript(writeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("行数 = %d，想要 3（第一问两次请求 + 第二问一次；sdk-cli 的那问不算）", len(turns))
	}

	// 第一问：req_A 在文件里重复出现只算一次，req_B 另成一行，两行共享回合 ID。
	first, second := turns[0], turns[1]
	if first.model != "claude-sonnet-5" || second.model != "claude-sonnet-5" {
		t.Errorf("模型 = %q / %q", first.model, second.model)
	}
	if first.ev.OutputTokens != 100 || second.ev.OutputTokens != 50 {
		t.Errorf("输出 = %d / %d，想要 100 / 50（req_A 的重复记录不重复计）", first.ev.OutputTokens, second.ev.OutputTokens)
	}
	if first.ev.CacheReadTokens != 1000 || first.ev.CacheWriteTokens != 200 || second.ev.CacheReadTokens != 1200 {
		t.Errorf("缓存 = %+v / %+v", first.ev, second.ev)
	}
	// 第一次请求的键就是旧版合并行的键（回合首个 requestId），重扫时原地覆盖旧行。
	if first.reqID != "req_A" || second.reqID != "req_B" || first.ev.TurnID != "req_A" || second.ev.TurnID != "req_A" {
		t.Errorf("去重键/回合 ID = %q,%q / %q,%q", first.reqID, second.reqID, first.ev.TurnID, second.ev.TurnID)
	}
	if first.ts.Format("15:04:05") != "10:00:02" || second.ts.Format("15:04:05") != "10:00:05" {
		t.Errorf("请求时间 = %s / %s，想要各自最早那条记录", first.ts.Format("15:04:05"), second.ts.Format("15:04:05"))
	}

	// 第二问换了模型，是另一个回合。
	if turns[2].model != "claude-haiku-4-5" || turns[2].ev.TurnID != "req_D" || turns[2].reqID != "req_D" {
		t.Errorf("第二个回合 = %q/%q", turns[2].model, turns[2].ev.TurnID)
	}

	// sdk-cli 的量一个 token 都不能漏进来，漏了就是对话消耗记两遍。
	for _, tr := range turns {
		if tr.reqID == "req_Z" || tr.ev.OutputTokens == 900 {
			t.Error("把 sdk-cli（我们自己发起的对话）的消耗也补记了，会重复计费")
		}
	}
}

// 扫描是反复跑的：文件没变也好、变长了也好，同一个回合只能有一行，
// 而且续写进来的新调用要能加到已有那行上。
func TestUpsertTerminalUsageIsIdempotent(t *testing.T) {
	s, _ := newTestServer(t)
	row := store.UsageEvent{
		User: "alice", SessionID: "s1", TurnID: "req_A", ReqID: "req_A",
		Agent: "claude", Model: "claude-sonnet-5",
		InputTokens: 4, OutputTokens: 100, CostMicroUSD: 0,
	}
	for range 3 {
		if err := s.store.UpsertTerminalUsage(row); err != nil {
			t.Fatal(err)
		}
	}
	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	if len(rows) != 1 {
		t.Fatalf("扫了三遍留下 %d 行，想要 1（req_id 唯一索引没生效）", len(rows))
	}
	if rows[0].Kind != store.UsageKindTerminal {
		t.Errorf("kind = %q，想要 terminal", rows[0].Kind)
	}

	// 回合还在进行时先落了一行，下一轮扫到更多调用，得更新而不是新增。
	row.OutputTokens = 150
	if err := s.store.UpsertTerminalUsage(row); err != nil {
		t.Fatal(err)
	}
	rows = s.store.ListUsage(store.UsageFilter{User: "alice"})
	if len(rows) != 1 || rows[0].OutputTokens != 150 {
		t.Fatalf("续写后 %d 行、输出 %d，想要 1 行、150", len(rows), rows[0].OutputTokens)
	}
}

// 端到端走一遍扫描：transcript 摆在会话 home 的真实位置上，扫完要能在流水里
// 查到终端行。只扫库里存在的会话——磁盘上已删除会话的残留目录归不到人头上。
func TestScanTerminalUsage(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	sess.AccountID = "claude-1"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := strings.ReplaceAll(termFixture, `"type":"assistant",`, `"type":"assistant","effort":"xhigh",`)
	if err := os.WriteFile(filepath.Join(dir, "abc.jsonl"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	sc := newScanner()
	s.scanTerminalUsage(sc)

	rows := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 3 {
		t.Fatalf("终端行数 = %d，想要 3", len(rows))
	}
	for _, r := range rows {
		if r.User != sess.User || r.SessionID != sess.ID || r.AccountID != sess.AccountID {
			t.Errorf("归属错了：%+v", r)
		}
		if r.Effort != "xhigh" || r.EffortControl != "effort" {
			t.Errorf("transcript effort not persisted: %+v", r)
		}
	}

	// 文件没动过就不该再读一遍；再扫一次也不许多出行来（req_id 兜底）。
	s.scanTerminalUsage(sc)
	if again := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal}); len(again) != 3 {
		t.Fatalf("再扫一遍变成 %d 行，想要还是 3", len(again))
	}
}

// 终端消耗只记账不扣额度：补记是异步的、价还是我们自己查表折的，
// 拿它去动余额用户没法对账。拦终端靠的是余额见底不让开终端，不是这里扣钱。
func TestTerminalUsageNeverCharges(t *testing.T) {
	s, _ := newTestServer(t)
	const user = "alice"
	if _, err := s.store.Grant(user, 1_000_000, "test:seed", "充值", "root"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.store.GetQuota(user)
	if before.BalanceMicroUSD != 1_000_000 {
		t.Fatalf("初始余额 = %d，想要 1000000", before.BalanceMicroUSD)
	}

	if err := s.store.UpsertTerminalUsage(store.UsageEvent{
		User: user, SessionID: "s1", TurnID: "req_A", ReqID: "req_A",
		Agent: "claude", Model: "claude-sonnet-5",
		InputTokens: 4, OutputTokens: 100, CostMicroUSD: 250_000,
	}); err != nil {
		t.Fatal(err)
	}

	after, _ := s.store.GetQuota(user)
	if after.BalanceMicroUSD != before.BalanceMicroUSD {
		t.Errorf("余额从 %d 变成 %d，终端消耗不该扣额度",
			before.BalanceMicroUSD, after.BalanceMicroUSD)
	}
}

// 升级前终端行按 (回合, 模型) 合并、键是首个 requestId。升级后重扫同一份
// transcript：第一次请求原地覆盖旧合并行，其余请求新增成行并沿用旧行的价格
// 快照，合计不变、不重复。
func TestTerminalMergedRowsConvertInPlace(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	old := &store.PriceSnapshot{Version: 1, Source: "table", Key: "claude", Standard: store.TokenRates{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1}}
	merged := store.UsageEvent{User: sess.User, SessionID: sess.ID, TurnID: "req_A", ReqID: "req_A", Agent: "claude", Model: "claude-sonnet-5",
		InputTokens: 6, OutputTokens: 150, CacheReadTokens: 2200, CacheWriteTokens: 200, Price: old}
	merged.CostMicroUSD = old.Cost(merged)
	if err := s.store.UpsertTerminalUsage(merged); err != nil {
		t.Fatal(err)
	}
	// Today's table differs; converted rows must keep the turn's snapshot.
	s.cfg.Pricing = map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 99, Output: 99}}}
	dir := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "abc.jsonl"), []byte(termFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s.scanTerminalUsage(newScanner())
	var sonnet []store.UsageEvent
	for _, r := range s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal, Asc: true}) {
		if r.Model == "claude-sonnet-5" {
			sonnet = append(sonnet, r)
		}
	}
	if len(sonnet) != 2 || sonnet[0].OutputTokens != 100 || sonnet[1].OutputTokens != 50 || sonnet[0].TurnID != "req_A" || sonnet[1].TurnID != "req_A" {
		t.Fatalf("merged row not converted: %+v", sonnet)
	}
	if sonnet[0].Price.Standard.Input != 1 || sonnet[1].Price.Standard.Input != 1 || sonnet[0].CostMicroUSD+sonnet[1].CostMicroUSD != merged.CostMicroUSD {
		t.Fatalf("conversion repriced: %d + %d != %d", sonnet[0].CostMicroUSD, sonnet[1].CostMicroUSD, merged.CostMicroUSD)
	}
}
