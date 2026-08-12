package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	// 三行分属两个回合：claude 那两行共享 turn_id。
	if got.Total.Rows != 3 || got.Total.Turns != 2 {
		t.Errorf("合计 = %d 行 / %d 回合，想要 3 / 2", got.Total.Rows, got.Total.Turns)
	}
	if want := int64(147633 + 622 + 3250); got.Total.CostMicroUSD != want {
		t.Errorf("合计费用 = %d，想要 %d", got.Total.CostMicroUSD, want)
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
	tally.observe(store.UsageEvent{User: "alice", SessionID: "s1", TurnID: "t9",
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
