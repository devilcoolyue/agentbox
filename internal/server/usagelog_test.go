package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// usageEventsResp 只解出断言要用的部分。
type usageEventsResp struct {
	Rows  []usageRowView    `json:"rows"`
	Total store.UsageTotals `json:"total"`
	Facet struct {
		Users  []string `json:"users"`
		Agents []string `json:"agents"`
		Models []string `json:"models"`
	} `json:"facets"`
	Scope string `json:"scope"`
	Limit int    `json:"limit"`
	Order string `json:"order"`
	Zone  string `json:"timezone"`
}

// seedUsage 铺一批流水：alice 一个 claude 回合拆成两行（主模型 + 起标题的
// haiku），bob 一行 codex。turn_id 的共享是故意的，回合数去重要靠它。
func seedUsage(t *testing.T, s *Server) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	evs := []store.UsageEvent{
		{TS: base, User: "alice", SessionID: "s1", TurnID: "t1", Agent: "claude",
			Model: "claude-opus-5", Kind: store.UsageKindChat, Provider: "firstParty",
			InputTokens: 4, OutputTokens: 2836, CacheReadTokens: 22306,
			CacheWriteTokens: 6556, CostMicroUSD: 147633, DurationMS: 53509, TTFTMs: 1200},
		{TS: base, User: "alice", SessionID: "s1", TurnID: "t1", Agent: "claude",
			Model: "claude-haiku-4-5", Kind: store.UsageKindChat,
			InputTokens: 532, OutputTokens: 18, CostMicroUSD: 622},
		{TS: base.Add(time.Minute), User: "bob", SessionID: "s2", TurnID: "t2",
			Agent: "codex", Model: "gpt-5.5-codex", Kind: store.UsageKindChat,
			InputTokens: 1000, OutputTokens: 100, CostMicroUSD: 3250},
	}
	if err := s.store.InsertUsage(evs...); err != nil {
		t.Fatal(err)
	}
}

func getUsageEvents(t *testing.T, s *Server, url, user, role string) usageEventsResp {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleUsageEvents(w, asUser(httptest.NewRequest(http.MethodGet, url, nil), user, role))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d：%s", w.Code, w.Body.String())
	}
	var got usageEventsResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// 管理员看得见所有人的流水，合计按整个筛选范围算。
func TestUsageEventsAdminSeesEveryone(t *testing.T) {
	s, _ := newTestServer(t)
	seedUsage(t, s)

	got := getUsageEvents(t, s, "/api/usage/events", "root", store.RoleAdmin)
	if len(got.Rows) != 3 {
		t.Fatalf("行数 = %d，想要 3", len(got.Rows))
	}
	if got.Scope != "all" {
		t.Errorf("scope = %q，管理员应该是 all", got.Scope)
	}
	if got.Zone != config.DefaultTimeZone {
		t.Errorf("timezone = %q，想要默认值 %q", got.Zone, config.DefaultTimeZone)
	}
	// 三行分属两个回合：claude 那两行共享 turn_id。
	if got.Total.Rows != 3 || got.Total.Turns != 2 {
		t.Errorf("合计 = %d 行 / %d 回合，想要 3 / 2", got.Total.Rows, got.Total.Turns)
	}
	if want := int64(147633 + 622 + 3250); got.Total.CostMicroUSD != want {
		t.Errorf("合计费用 = %d，想要 %d", got.Total.CostMicroUSD, want)
	}
}

func TestUsageFilterWallClockUsesConfiguredTimeZone(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.TimeZone = "Asia/Shanghai"
	r := asUser(httptest.NewRequest(http.MethodGet,
		"/api/usage/events?since=2026-09-19T08:30&until=2026-09-19T09:01", nil),
		"root", store.RoleAdmin)

	f, _ := s.usageFilterFrom(r)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	wantSince := time.Date(2026, 9, 19, 8, 30, 0, 0, loc)
	wantUntil := time.Date(2026, 9, 19, 9, 1, 0, 0, loc)
	if !f.Since.Equal(wantSince) || !f.Until.Equal(wantUntil) {
		t.Fatalf("墙上时间解析错误: since=%s until=%s, want %s / %s",
			f.Since, f.Until, wantSince, wantUntil)
	}

	// 带偏移的旧客户端参数仍按它自己的绝对时间解释。
	r = asUser(httptest.NewRequest(http.MethodGet,
		"/api/usage/events?since=2026-09-19T08:30:00-07:00", nil),
		"root", store.RoleAdmin)
	f, _ = s.usageFilterFrom(r)
	wantRFC3339 := time.Date(2026, 9, 19, 15, 30, 0, 0, time.UTC)
	if !f.Since.Equal(wantRFC3339) {
		t.Fatalf("RFC3339 兼容解析错误: got %s want %s", f.Since, wantRFC3339)
	}
}

func TestUsageEventsYesterdayExcludesTodayInShanghai(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.TimeZone = "Asia/Shanghai"
	for _, e := range []struct {
		stamp string
		turn  string
	}{
		{"2026-09-22T15:59:00Z", "before"}, // 09/22 23:59 Shanghai
		{"2026-09-22T16:00:00Z", "start"},  // 09/23 00:00 Shanghai
		{"2026-09-23T15:59:00Z", "end"},    // 09/23 23:59 Shanghai
		{"2026-09-23T17:04:00Z", "today"},  // 09/24 01:04 Shanghai
	} {
		ts, err := time.Parse(time.RFC3339, e.stamp)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.store.InsertUsage(store.UsageEvent{TS: ts, User: "alice", SessionID: "s1",
			TurnID: e.turn, Agent: "claude", CostMicroUSD: 100}); err != nil {
			t.Fatal(err)
		}
	}
	got := getUsageEvents(t, s,
		"/api/usage/events?since=2026-09-23T00:00&until=2026-09-24T00:00",
		"root", store.RoleAdmin)
	if got.Total.Rows != 2 || len(got.Rows) != 2 || got.Total.CostMicroUSD != 200 {
		t.Fatalf("昨日筛选结果 = %+v，想要两条 09/23 记录", got)
	}
	if got.Rows[0].TurnID != "end" || got.Rows[1].TurnID != "start" {
		t.Fatalf("昨日记录或顺序错误: %+v", got.Rows)
	}
}

// 普通用户只看得到自己的，连别人的用户名都不该从筛选项里漏出去——
// facets 会为了「选了还能改回来」放宽 user 过滤，边界必须另外钉死。
func TestUsageEventsUserScopedIncludingFacets(t *testing.T) {
	s, _ := newTestServer(t)
	seedUsage(t, s)

	// 明着传别人的用户名也没用。
	got := getUsageEvents(t, s, "/api/usage/events?user=bob", "alice", store.RoleUser)
	if got.Scope != "self" {
		t.Errorf("scope = %q，普通用户应该是 self", got.Scope)
	}
	for _, r := range got.Rows {
		if r.User != "alice" {
			t.Fatalf("返回了 %s 的流水，普通用户只能看自己的", r.User)
		}
	}
	if got.Total.Rows != 2 || got.Total.Turns != 1 {
		t.Errorf("合计 = %d 行 / %d 回合，想要 2 / 1", got.Total.Rows, got.Total.Turns)
	}
	for _, u := range got.Facet.Users {
		if u != "alice" {
			t.Fatalf("筛选项里出现了 %q，普通用户不该知道别人存在", u)
		}
	}
	for _, m := range got.Facet.Models {
		if m == "gpt-5.5-codex" {
			t.Fatal("筛选项里出现了别人用过的模型")
		}
	}
}

// 选中一个模型之后，模型下拉里仍要有其他模型，否则选完就回不去了。
// 同时确认非自身列（agent）会跟着筛选收窄。
func TestUsageEventsFacetsRelaxOwnColumn(t *testing.T) {
	s, _ := newTestServer(t)
	seedUsage(t, s)

	got := getUsageEvents(t, s, "/api/usage/events?model=claude-opus-5", "root", store.RoleAdmin)
	if len(got.Rows) != 1 {
		t.Fatalf("行数 = %d，想要 1", len(got.Rows))
	}
	if len(got.Facet.Models) != 3 {
		t.Errorf("模型筛选项 = %v，选中一个之后其余的必须还在", got.Facet.Models)
	}
	if len(got.Facet.Agents) != 1 || got.Facet.Agents[0] != "claude" {
		t.Errorf("agent 筛选项 = %v，想要只剩 claude", got.Facet.Agents)
	}
}

// 翻页只影响 rows，不影响合计——否则表头的总花费会随翻页变化。
func TestUsageEventsPagingKeepsTotals(t *testing.T) {
	s, _ := newTestServer(t)
	seedUsage(t, s)

	first := getUsageEvents(t, s, "/api/usage/events?limit=1", "root", store.RoleAdmin)
	second := getUsageEvents(t, s, "/api/usage/events?limit=1&offset=1", "root", store.RoleAdmin)
	if len(first.Rows) != 1 || len(second.Rows) != 1 {
		t.Fatalf("每页行数 = %d / %d，想要 1 / 1", len(first.Rows), len(second.Rows))
	}
	if first.Rows[0].ID == second.Rows[0].ID {
		t.Error("两页拿到同一行，offset 没生效")
	}
	if first.Total.CostMicroUSD != second.Total.CostMicroUSD || first.Total.Rows != 3 {
		t.Errorf("合计随翻页变了：%d vs %d", first.Total.CostMicroUSD, second.Total.CostMicroUSD)
	}
}

// 默认最新在最前；order=asc 整个翻过来。两头的第一行必须是对方的最后一行，
// 否则「正序」只是把当前这一页倒过来排，跨页看仍是乱的。
func TestUsageEventsOrder(t *testing.T) {
	s, _ := newTestServer(t)
	seedUsage(t, s)

	desc := getUsageEvents(t, s, "/api/usage/events", "root", store.RoleAdmin)
	if desc.Order != "desc" {
		t.Errorf("默认 order = %q，想要 desc", desc.Order)
	}
	for i := 1; i < len(desc.Rows); i++ {
		if desc.Rows[i-1].TS < desc.Rows[i].TS {
			t.Fatalf("默认没按时间倒排：第 %d 行 %d 早于上一行 %d", i, desc.Rows[i].TS, desc.Rows[i-1].TS)
		}
	}

	asc := getUsageEvents(t, s, "/api/usage/events?order=asc", "root", store.RoleAdmin)
	if asc.Order != "asc" {
		t.Errorf("order = %q，想要 asc", asc.Order)
	}
	if len(asc.Rows) != len(desc.Rows) {
		t.Fatalf("换个顺序行数就变了：%d vs %d", len(asc.Rows), len(desc.Rows))
	}
	for i := 1; i < len(asc.Rows); i++ {
		if asc.Rows[i-1].TS > asc.Rows[i].TS {
			t.Fatalf("order=asc 没按时间正排：第 %d 行 %d 晚于上一行 %d", i, asc.Rows[i].TS, asc.Rows[i-1].TS)
		}
	}
	if asc.Rows[0].ID != desc.Rows[len(desc.Rows)-1].ID {
		t.Errorf("正序第一行 id=%d，想要倒序的最后一行 id=%d",
			asc.Rows[0].ID, desc.Rows[len(desc.Rows)-1].ID)
	}
	// 合计只跟筛选条件有关，排序不该动它。
	if asc.Total.CostMicroUSD != desc.Total.CostMicroUSD || asc.Total.Rows != desc.Total.Rows {
		t.Errorf("合计随排序变了：%+v vs %+v", asc.Total, desc.Total)
	}

	// 只认小写的 asc，别的值一律走默认倒序——前端只会发小写，这里不做花式解析。
	if got := getUsageEvents(t, s, "/api/usage/events?order=ASC", "root", store.RoleAdmin); got.Order != "desc" {
		t.Errorf("order=ASC 时 = %q，无法识别的值应该退回 desc", got.Order)
	}
}

// 首字与总耗时必须出自同一块表：都从容器就绪开始算，所以墙钟不可能比首字小。
// provider 自报的 duration_ms 只算模型侧、不含 CLI 启动的那两秒，比首字小是它
// 的正常状态——线上真出现过「首字 3.1s / 总耗时 2.3s」，就是拿它当了总耗时。
// 回合拆成几行，每行都得带上同一个墙钟。
func TestFlushUsageStampsWallClock(t *testing.T) {
	s, _ := newTestServer(t)
	r := &chatRoom{srv: s, sessID: "s1"}

	const ttft, wall, providerDur = 3099, 4530, 2314
	line := []byte(`{"type":"result","duration_ms":` + itoa(providerDur) + `,"total_cost_usd":0.0604,
		"modelUsage":{
			"claude-sonnet-5":{"inputTokens":2,"outputTokens":26,"costUSD":0.0598,"provider":"firstParty"},
			"claude-haiku-4-5":{"inputTokens":532,"outputTokens":18,"costUSD":0.0006}}}`)

	var tally usageTally
	for _, message := range []string{
		`{"type":"assistant","message":{"id":"msg-sonnet","model":"claude-sonnet-5","usage":{"input_tokens":2,"output_tokens":26}}}`,
		`{"type":"assistant","message":{"id":"msg-haiku","model":"claude-haiku-4-5","usage":{"input_tokens":532,"output_tokens":18}}}`,
	} {
		tally.Observe(store.UsageEvent{User: "alice", SessionID: "s1", TurnID: "t9", Agent: "claude", Kind: store.UsageKindChat, TTFTMs: ttft}, []byte(message))
	}
	tally.Observe(store.UsageEvent{User: "alice", SessionID: "s1", TurnID: "t9",
		Agent: "claude", Kind: store.UsageKindChat, TTFTMs: ttft}, line)
	if n := r.flushUsage(&tally, wall*time.Millisecond); n != 2 {
		t.Fatalf("落库行数 = %d，想要 2（回合按模型拆两行）", n)
	}

	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	if len(rows) != 2 {
		t.Fatalf("读回 %d 行，想要 2", len(rows))
	}
	for _, e := range rows {
		if e.WallMS != wall {
			t.Errorf("%s: wall_ms = %d，想要 %d（回合各行共用同一个墙钟）", e.Model, e.WallMS, wall)
		}
		if e.DurationMS != providerDur {
			t.Errorf("%s: duration_ms = %d，想要原样保留 provider 的 %d", e.Model, e.DurationMS, providerDur)
		}
		if e.WallMS < e.TTFTMs {
			t.Errorf("%s: 墙钟 %d 小于首字 %d，两个数没同源", e.Model, e.WallMS, e.TTFTMs)
		}
	}
}

// 计费模式是给人解释「这个数怎么来的」，claude 一律算 provider 报价——
// 哪怕这一行的价是 0，也不能说成「没定价」。
func TestBillingMode(t *testing.T) {
	cases := []struct {
		name string
		ev   store.UsageEvent
		want string
	}{
		{"claude 有价", store.UsageEvent{Agent: "claude", CostMicroUSD: 100}, billingProvider},
		{"claude 报 0", store.UsageEvent{Agent: "claude"}, billingProvider},
		{"codex 查到价", store.UsageEvent{Agent: "codex", CostMicroUSD: 3250}, billingTable},
		{"codex 没配价", store.UsageEvent{Agent: "codex"}, billingNone},
	}
	for _, c := range cases {
		if got := billingMode(c.ev); got != c.want {
			t.Errorf("%s：计费模式 = %q，想要 %q", c.name, got, c.want)
		}
	}
}

// 一页最多给多少行由服务端说了算，别让 limit 参数拖走整张表。
func TestUsageEventsLimitCapped(t *testing.T) {
	s, _ := newTestServer(t)
	seedUsage(t, s)

	got := getUsageEvents(t, s, "/api/usage/events?limit=100000", "root", store.RoleAdmin)
	if got.Limit != usageRowsMax {
		t.Errorf("limit = %d，想要被压到 %d", got.Limit, usageRowsMax)
	}
	if len(got.Rows) != 3 {
		t.Errorf("行数 = %d，想要 3", len(got.Rows))
	}
}

// 费用明细要能说清「这一行按哪条价、哪一档算的」：查表的行给 table，provider
// 自报价的行只能给照表推的参考拆分（reference），两者不能混为一谈。
func TestRateForBasisAndTier(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{
		"codex": {
			TokenRates:      config.TokenRates{Input: 1.25, Output: 10, CacheRead: 0.125},
			LongContextOver: 272000,
			Long:            &config.TokenRates{Input: 2.5, Output: 20, CacheRead: 0.25},
		},
		"claude-opus-5": {TokenRates: config.TokenRates{Input: 5, Output: 25}},
	}

	// 没有精确的模型行就按 agent 兜底；输入没过阈值，走短上下文档。
	got := s.rateFor(store.UsageEvent{Agent: "codex", Model: "gpt-5.5-codex",
		InputTokens: 1000, CacheReadTokens: 8000, CostMicroUSD: 3250})
	if got == nil || got.Key != "codex" || got.Basis != rateBasisTable || got.Long {
		t.Fatalf("codex 行 = %+v，想要 codex 兜底 / table / 短上下文档", got)
	}
	if got.Input != 1.25 {
		t.Errorf("输入单价 = %v，想要 1.25", got.Input)
	}

	// 档位按「输入 + 缓存读取」判定，与 priceEvent 同口径。
	long := s.rateFor(store.UsageEvent{Agent: "codex", Model: "gpt-5.5-codex",
		InputTokens: 100000, CacheReadTokens: 200000})
	if long == nil || !long.Long || long.Input != 2.5 || long.Over != 272000 {
		t.Fatalf("过阈值的行 = %+v，想要长上下文档单价", long)
	}

	// claude 的对话行以 provider 自报的总额为准，价目表只能当参考。
	ref := s.rateFor(store.UsageEvent{Agent: "claude", Model: "claude-opus-5",
		Kind: store.UsageKindChat, CostMicroUSD: 147633})
	if ref == nil || ref.Basis != rateBasisReference {
		t.Fatalf("claude 对话行 = %+v，想要 reference", ref)
	}

	// 终端行是事后从 transcript 补记的，那里只有 token，claude 也只能查表。
	term := s.rateFor(store.UsageEvent{Agent: "claude", Model: "claude-opus-5",
		Kind: store.UsageKindTerminal})
	if term == nil || term.Basis != rateBasisTable {
		t.Fatalf("终端行 = %+v，想要 table", term)
	}

	// 表里没有的模型不给单价，前端据此显示「未定价」。
	if v := s.rateFor(store.UsageEvent{Agent: "gemini", Model: "x"}); v != nil {
		t.Errorf("查不到价时 = %+v，想要 nil", v)
	}
}

func TestRateUsesSavedSnapshotAfterPriceEdit(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 2}}}
	e := s.usageService().Price(store.UsageEvent{Agent: "codex", InputTokens: 10})
	s.cfg.Pricing["codex"] = config.ModelPrice{TokenRates: config.TokenRates{Input: 99}}
	rate := s.rateFor(e)
	if rate == nil || !rate.Snapshot || rate.Input != 2 || billingMode(e) != "table" {
		t.Fatalf("historical price view: %+v", rate)
	}
	e.Price = nil
	if rate = s.rateFor(e); rate == nil || rate.Snapshot || rate.Input != 99 {
		t.Fatal("legacy reference changed")
	}
}

// 网页回合的思考强度取自它的聊天回执；终端、起标题和没有回执的回合留空。
func TestUsageEventsShowTheTurnsEffort(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.store.CreateUser(store.User{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.store.GetUser("alice")
	if err := s.store.Put(store.Session{ID: "space", User: u.Name}); err != nil {
		t.Fatal(err)
	}
	c, _, err := s.store.AcceptChatRequest(u, "space", "request", store.ChatRequestInput{Scope: "scope", ThreadID: "thread", Text: "synthetic", Model: "claude-opus-5-5", Effort: "xhigh", EffortControl: "effort"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.store.InsertUsage(
		store.UsageEvent{TS: now, User: "alice", SessionID: "space", TurnID: c.TurnID, Agent: "claude", Model: "claude-opus-5-5", Kind: store.UsageKindChat, InputTokens: 1},
		store.UsageEvent{TS: now, User: "alice", SessionID: "space", TurnID: c.TurnID, Agent: "claude", Model: "claude-haiku-5-5", Kind: store.UsageKindTitle, InputTokens: 1},
		store.UsageEvent{TS: now, User: "alice", SessionID: "space", TurnID: "legacy", Agent: "claude", Model: "claude-opus-5-5", Kind: store.UsageKindChat, InputTokens: 1},
	); err != nil {
		t.Fatal(err)
	}
	got := getUsageEvents(t, s, "/api/usage/events", "alice", store.RoleUser)
	efforts := map[string]string{}
	for _, r := range got.Rows {
		efforts[r.Kind+"/"+r.TurnID] = r.Effort + "/" + r.EffortControl
	}
	if efforts["chat/"+c.TurnID] != "xhigh/effort" || efforts["title/"+c.TurnID] != "/" || efforts["chat/legacy"] != "/" {
		t.Fatal(efforts)
	}
}

