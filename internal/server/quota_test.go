package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// asUser 把请求身份塞进 context，绕开 auth 中间件直接调 handler。
func asUser(r *http.Request, name, role string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxUser,
		store.User{Name: name, Role: role}))
}

// --- 定价 ---

// claude 自己报了账单价，我们不去猜。
func TestPriceEventKeepsProviderCost(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{
		"claude-opus-4-8": {TokenRates: config.TokenRates{Input: 15, Output: 75}},
	}
	got := s.priceEvent(store.UsageEvent{
		Agent: "claude", Model: "claude-opus-4-8",
		InputTokens: 2, OutputTokens: 1870, CostMicroUSD: 103081,
	})
	if got != 103081 {
		t.Errorf("费用 = %d，provider 报的价必须原样保留", got)
	}
}

// codex 不报价，按价目表折算。这条同时锁住单位换算：价目表是「每百万 token
// 多少美元」，结果是微美元，两个 1e6 约掉后就是 tokens × rate。
func TestPriceEventCodexFromTable(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{
		"gpt-5.5-codex": {TokenRates: config.TokenRates{
			Input: 1.25, Output: 10, CacheRead: 0.125, CacheWrite: 1.25}},
	}
	got := s.priceEvent(store.UsageEvent{
		Agent: "codex", Model: "gpt-5.5-codex",
		InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 8000,
	})
	// 1000×1.25 + 100×10 + 8000×0.125 = 1250 + 1000 + 1000 = 3250 微美元
	if got != 3250 {
		t.Errorf("费用 = %d 微美元，想要 3250（$0.00325）", got)
	}
}

// codex 的事件常常不带模型名，这时按 agent 名兜底，否则这些回合永远免费。
func TestPriceEventFallsBackToAgent(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{
		"codex": {TokenRates: config.TokenRates{Input: 1.25, Output: 10}},
	}
	got := s.priceEvent(store.UsageEvent{
		Agent: "codex", Model: "", InputTokens: 1000, OutputTokens: 100,
	})
	if got != 1250+1000 {
		t.Errorf("费用 = %d，想要 2250", got)
	}
}

// 提示词超过 272k token 后整轮跳到长上下文价（翻倍），不是只对超出部分加价。
// 用真实的 gpt-5.5 价目：短 $5/$0.5/$30，长 $10/$1/$45。
func TestPriceEventLongContextTier(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{
		"gpt-5.5": {
			TokenRates:      config.TokenRates{Input: 5, CacheRead: 0.5, Output: 30},
			LongContextOver: 272_000,
			Long:            &config.TokenRates{Input: 10, CacheRead: 1, Output: 45},
		},
	}
	price := func(in, cached, out int64) int64 {
		return s.priceEvent(store.UsageEvent{
			Agent: "codex", Model: "gpt-5.5",
			InputTokens: in, CacheReadTokens: cached, OutputTokens: out,
		})
	}

	// 正好卡在线上（272000）算短上下文——门槛是「大于」。
	// 200000×5 + 72000×0.5 + 1000×30 = 1000000 + 36000 + 30000
	if got, want := price(200_000, 72_000, 1000), int64(1_066_000); got != want {
		t.Errorf("阈值上应按短上下文计价: %d，想要 %d", got, want)
	}
	// 多喂一个 token 就整轮跳档，两倍。
	// 200001×10 + 72000×1 + 1000×45 = 2000010 + 72000 + 45000
	if got, want := price(200_001, 72_000, 1000), int64(2_117_010); got != want {
		t.Errorf("过线应整轮按长上下文计价: %d，想要 %d", got, want)
	}
	// 档位看的是喂进去的总量（未命中 + 命中缓存），不是只看未命中那部分。
	// 缓存命中占大头的长会话最容易在这里被算便宜。
	if got := price(1000, 300_000, 100); got <= 0 {
		t.Fatal("价格算成了 0")
	}
	// 1000×10 + 300000×1 + 100×45 = 10000 + 300000 + 4500
	if got, want := price(1000, 300_000, 100), int64(314_500); got != want {
		t.Errorf("缓存命中也要计入档位判定: %d，想要 %d", got, want)
	}
}

// 没有长上下文档的模型（gpt-5.4-mini 那种），喂再多也用同一套价，不能崩。
func TestPriceEventNoLongTier(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Pricing = map[string]config.ModelPrice{
		"gpt-5.4-mini": {TokenRates: config.TokenRates{Input: 0.75, CacheRead: 0.075, Output: 4.5}},
	}
	got := s.priceEvent(store.UsageEvent{
		Agent: "codex", Model: "gpt-5.4-mini",
		InputTokens: 400_000, OutputTokens: 1000,
	})
	// 400000×0.75 + 1000×4.5 = 300000 + 4500
	if want := int64(304_500); got != want {
		t.Errorf("费用 = %d，想要 %d", got, want)
	}
}

// 没配价目表时记 0：只记不扣，绝不瞎猜一个价扣用户的钱。
func TestPriceEventWithoutTable(t *testing.T) {
	s, _ := newTestServer(t)
	got := s.priceEvent(store.UsageEvent{
		Agent: "codex", Model: "gpt-5.5-codex", InputTokens: 99999, OutputTokens: 99999,
	})
	if got != 0 {
		t.Errorf("费用 = %d，未配价目表时必须是 0", got)
	}
}

// --- 拦截策略 ---

func TestQuotaBlock(t *testing.T) {
	s, _ := newTestServer(t)

	// 没开额度 = 不限额。老部署升级上来全是这种用户。
	if why := s.quotaBlock("alice"); why != "" {
		t.Errorf("未开额度的用户被拦了: %s", why)
	}

	if _, err := s.store.Grant("alice", 1000, "grant:seed", "", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.SetQuotaEnforced("alice", true); err != nil {
		t.Fatal(err)
	}
	if why := s.quotaBlock("alice"); why != "" {
		t.Errorf("余额充足却被拦: %s", why)
	}

	// 超支到负数：下一个回合必须被拦住，且话要说人话。
	if err := s.store.InsertUsage(store.UsageEvent{
		User: "alice", SessionID: "s1", TurnID: "t1", Agent: "claude",
		CostMicroUSD: 103081,
	}); err != nil {
		t.Fatal(err)
	}
	why := s.quotaBlock("alice")
	if why == "" {
		t.Fatal("余额为负仍然放行")
	}
	if !strings.Contains(why, "-$0.1021") {
		t.Errorf("提示里应带上余额，得到 %q", why)
	}

	// 只计不拦：余额已经是负的，但不该挡住用户。
	if _, err := s.store.SetQuotaEnforced("alice", false); err != nil {
		t.Fatal(err)
	}
	if why := s.quotaBlock("alice"); why != "" {
		t.Errorf("enforced=false 时不该拦: %s", why)
	}
}

// --- 管理接口 ---

func TestHandleCreditGrantIdempotent(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: "x"}); err != nil {
		t.Fatal(err)
	}

	grant := func(ref string) map[string]any {
		body := `{"usd":2.5,"note":"首充","ref":"` + ref + `"}`
		r := httptest.NewRequest("POST", "/api/users/alice/credits", strings.NewReader(body))
		r.SetPathValue("name", "alice")
		w := httptest.NewRecorder()
		s.handleCreditGrant(w, asUser(r, "boxadmin", store.RoleAdmin))
		if w.Code != http.StatusOK {
			t.Fatalf("充值返回 %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := grant("order-42")
	if first["applied"] != true {
		t.Error("首次充值应生效")
	}
	second := grant("order-42")
	if second["applied"] != false {
		t.Error("同一 ref 重复提交不该再充一次")
	}
	q, _ := s.store.GetQuota("alice")
	if q.BalanceMicroUSD != 2_500_000 {
		t.Errorf("余额 = %d，想要 2500000（$2.50 只该进账一次）", q.BalanceMicroUSD)
	}
}

// 管理员填的 ref 会被加上 grant: 前缀，撞不掉消耗流水的 usage:<id> 幂等键。
func TestGrantRefCannotCollideWithUsage(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Grant("alice", 1_000_000, "grant:seed", "", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertUsage(store.UsageEvent{
		User: "alice", SessionID: "s1", TurnID: "t1", Agent: "claude", CostMicroUSD: 621,
	}); err != nil {
		t.Fatal(err)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: "alice"})
	if len(rows) != 1 {
		t.Fatalf("用量 %d 行", len(rows))
	}

	// 管理员故意填一个和消耗流水同名的 ref。
	body := `{"usd":1,"ref":"usage:` + itoa(int(rows[0].ID)) + `"}`
	r := httptest.NewRequest("POST", "/api/users/alice/credits", strings.NewReader(body))
	r.SetPathValue("name", "alice")
	w := httptest.NewRecorder()
	s.handleCreditGrant(w, asUser(r, "boxadmin", store.RoleAdmin))
	if w.Code != http.StatusOK {
		t.Fatalf("充值返回 %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Applied bool `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if !out.Applied {
		t.Fatal("充值被误判成重放，说明前缀没起作用")
	}
	q, _ := s.store.GetQuota("alice")
	if q.BalanceMicroUSD != 1_000_000-621+1_000_000 {
		t.Errorf("余额 = %d，想要 %d", q.BalanceMicroUSD, 1_000_000-621+1_000_000)
	}
}

func TestHandleQuotaSetToggleAndRemove(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: "x"}); err != nil {
		t.Fatal(err)
	}
	put := func(body string) quotaView {
		r := httptest.NewRequest("PUT", "/api/users/alice/quota", strings.NewReader(body))
		r.SetPathValue("name", "alice")
		w := httptest.NewRecorder()
		s.handleQuotaSet(w, asUser(r, "boxadmin", store.RoleAdmin))
		if w.Code != http.StatusOK {
			t.Fatalf("返回 %d: %s", w.Code, w.Body.String())
		}
		var v quotaView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	v := put(`{"metered":true,"enforced":true}`)
	if !v.Metered || !v.Enforced || !v.Blocked {
		t.Errorf("零余额下开启管控应立刻显示为被拦: %+v", v)
	}
	v = put(`{"metered":false}`)
	if v.Metered {
		t.Errorf("解除限额后 metered 应为 false: %+v", v)
	}
	if s.quotaBlock("alice") != "" {
		t.Error("解除限额后仍被拦")
	}
}

func TestHandleQuotaUnknownUser(t *testing.T) {
	s, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/api/users/nobody/quota", nil)
	r.SetPathValue("name", "nobody")
	w := httptest.NewRecorder()
	s.handleQuotaGet(w, asUser(r, "boxadmin", store.RoleAdmin))
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的用户应返回 404，得到 %d", w.Code)
	}
}

// 普通用户查报表只能看到自己的账，哪怕 URL 里点名要别人的。
func TestUsageReportScopedToSelf(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.store.InsertUsage(
		store.UsageEvent{User: "alice", SessionID: "s1", TurnID: "t1", Agent: "claude",
			Model: "claude-opus-4-8", InputTokens: 10, CostMicroUSD: 621},
		store.UsageEvent{User: "bob", SessionID: "s2", TurnID: "t2", Agent: "claude",
			Model: "claude-opus-4-8", InputTokens: 20, CostMicroUSD: 103081},
	); err != nil {
		t.Fatal(err)
	}

	get := func(name, role, query string) map[string]any {
		r := httptest.NewRequest("GET", "/api/usage?"+query, nil)
		w := httptest.NewRecorder()
		s.handleUsageReport(w, asUser(r, name, role))
		if w.Code != http.StatusOK {
			t.Fatalf("返回 %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	mine := get("alice", store.RoleUser, "user=bob")
	total := mine["total"].(map[string]any)
	if total["cost_micro_usd"].(float64) != 621 {
		t.Errorf("普通用户看到了别人的账: %v", total)
	}

	all := get("boxadmin", store.RoleAdmin, "")
	total = all["total"].(map[string]any)
	if total["cost_micro_usd"].(float64) != 621+103081 {
		t.Errorf("管理员应看到全量: %v", total)
	}
	if n := len(all["by_user"].([]any)); n != 2 {
		t.Errorf("by_user 应有 2 个用户，得到 %d", n)
	}
	// 同一回合拆多行时回合数按 turn_id 去重，不能按行数算。
	if total["turns"].(float64) != 2 {
		t.Errorf("回合数 = %v，想要 2", total["turns"])
	}
}
