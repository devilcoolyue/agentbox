package server

import (
	"os"
	"path/filepath"
	"testing"

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

func TestParseTerminalTranscript(t *testing.T) {
	turns, err := parseTerminalTranscript(writeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("回合数 = %d，想要 2（终端两问；sdk-cli 的那问不算）", len(turns))
	}

	// 第一问：req_A 重复出现只算一次，加上 req_B 才是这一回合的总量。
	got := turns[0]
	if got.model != "claude-sonnet-5" {
		t.Errorf("模型 = %q", got.model)
	}
	if got.ev.OutputTokens != 150 {
		t.Errorf("输出 = %d，想要 150（100+50，req_A 的重复记录不重复计）", got.ev.OutputTokens)
	}
	if got.ev.CacheReadTokens != 2200 || got.ev.CacheWriteTokens != 200 {
		t.Errorf("缓存 = 读 %d / 写 %d，想要 2200 / 200", got.ev.CacheReadTokens, got.ev.CacheWriteTokens)
	}
	if got.reqID != "req_A" || got.ev.TurnID != "req_A" {
		t.Errorf("去重键/回合 ID = %q/%q，想要都是首个 requestId req_A", got.reqID, got.ev.TurnID)
	}
	if got.ts.Format("15:04:05") != "10:00:02" {
		t.Errorf("回合时间 = %s，想要回合里最早那次调用 10:00:02", got.ts.Format("15:04:05"))
	}

	// 第二问换了模型，必须是另一个回合、另一个去重键。
	if turns[1].model != "claude-haiku-4-5" || turns[1].ev.TurnID != "req_D" {
		t.Errorf("第二个回合 = %q/%q，想要 claude-haiku-4-5/req_D", turns[1].model, turns[1].ev.TurnID)
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
	if err := os.WriteFile(filepath.Join(dir, "abc.jsonl"), []byte(termFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	sc := &termScanner{seen: map[string]termFileState{}}
	s.scanTerminalUsage(sc)

	rows := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 2 {
		t.Fatalf("终端行数 = %d，想要 2", len(rows))
	}
	for _, r := range rows {
		if r.User != sess.User || r.SessionID != sess.ID || r.AccountID != sess.AccountID {
			t.Errorf("归属错了：%+v", r)
		}
	}

	// 文件没动过就不该再读一遍；再扫一次也不许多出行来（req_id 兜底）。
	s.scanTerminalUsage(sc)
	if again := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal}); len(again) != 2 {
		t.Fatalf("再扫一遍变成 %d 行，想要还是 2", len(again))
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
