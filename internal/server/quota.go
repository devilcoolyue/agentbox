package server

// 额度：定价、拦截、管理接口。落库与扣减本身在 store/quota.go，这里只管
// 「花了多少钱」和「还让不让花」。
//
// 策略（三条，改之前先读完）：
//
//   - **没开额度的用户不受限**。store 里没有 quotas 行 = 不限额，用量照记、
//     不扣钱。老部署升级上来没有任何一行，不会有人突然被拦在门外。
//   - **拦在回合开始前，不拦回合中间**。一个回合花多少钱要等 provider 在收尾
//     事件里报出来才知道，中途没有可靠的累计值。所以余额见底的那一个回合允许
//     超支，余额会变成负数，下一个回合才被拦住。这是有意的：宁可多花一个回合
//     的钱，也不要把用户正在跑的任务腰斩在一半。
//   - **claude 的价以 provider 报的为准**，只有不报价的（codex）才用配置里的
//     价目表按 token 折算。我们没有理由比 provider 更懂自己的账单。

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"agentbox/internal/store"
)

// priceEvent 给 provider 不报价的用量行折算费用，返回微美元。已经带价的行原样
// 返回；查不到价目表时返回 0——记账照旧，只是这行不产生扣减。
func (s *Server) priceEvent(ev store.UsageEvent) int64 {
	return s.usageService().Price(ev).CostMicroUSD
}

// quotaBlock 返回该用户此刻被拦的原因；空串表示放行。
func (s *Server) quotaBlock(user string) string {
	q, ok := s.store.GetQuota(user)
	if !ok || !q.Enforced || q.BalanceMicroUSD > 0 {
		return ""
	}
	return "额度已用完（余额 " + formatUSD(q.BalanceMicroUSD) + "），请联系管理员充值后再继续"
}

// formatUSD 把微美元转成给人看的金额。四位小数：单个便宜回合也就几百微美元，
// 两位小数会全部显示成 $0.00。负号放在 $ 前面（-$0.10 而不是 $-0.10）。
func formatUSD(micro int64) string {
	if micro < 0 {
		return "-" + formatUSD(-micro)
	}
	return fmt.Sprintf("$%.4f", float64(micro)/1e6)
}

// --- 管理接口 ---

type quotaView struct {
	User string `json:"user"`
	// Metered 为 false 表示该用户没有额度行，即不限额；此时其余金额字段无意义。
	Metered         bool  `json:"metered"`
	Enforced        bool  `json:"enforced"`
	Blocked         bool  `json:"blocked"`
	BalanceMicroUSD int64 `json:"balance_micro_usd"`
	GrantedMicroUSD int64 `json:"granted_micro_usd"`
	SpentMicroUSD   int64 `json:"spent_micro_usd"`
	UpdatedAt       int64 `json:"updated_at,omitempty"`
}

func viewQuota(user string, q store.Quota, metered bool) quotaView {
	if !metered {
		return quotaView{User: user}
	}
	return quotaView{
		User: user, Metered: true, Enforced: q.Enforced,
		Blocked:         q.Enforced && q.BalanceMicroUSD <= 0,
		BalanceMicroUSD: q.BalanceMicroUSD,
		GrantedMicroUSD: q.GrantedMicroUSD,
		SpentMicroUSD:   q.SpentMicroUSD,
		UpdatedAt:       q.UpdatedAt.UnixMilli(),
	}
}

// userQuotaView 读出一个用户的额度视图，顺带确认用户存在。
func (s *Server) userQuotaView(name string) (quotaView, bool) {
	if _, ok := s.store.GetUser(name); !ok {
		return quotaView{}, false
	}
	q, metered := s.store.GetQuota(name)
	return viewQuota(name, q, metered), true
}

type ledgerView struct {
	TS            int64  `json:"ts"`
	Ref           string `json:"ref"`
	Reason        string `json:"reason"`
	DeltaMicroUSD int64  `json:"delta_micro_usd"`
	BalanceAfter  int64  `json:"balance_after"`
	Note          string `json:"note,omitempty"`
	Actor         string `json:"actor,omitempty"`
}

// handleQuotaGet 返回额度状态与最近的账本流水。
func (s *Server) handleQuotaGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	view, ok := s.userQuotaView(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	entries := []ledgerView{}
	for _, e := range s.store.ListLedger(store.LedgerFilter{User: name, Limit: 100}) {
		entries = append(entries, ledgerView{
			TS: e.TS.UnixMilli(), Ref: e.Ref, Reason: e.Reason,
			DeltaMicroUSD: e.DeltaMicroUSD, BalanceAfter: e.BalanceAfter,
			Note: e.Note, Actor: e.Actor,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": view, "ledger": entries})
}

// handleQuotaSet 开关一个用户的额度管控。
//
//	{"metered": false}                 → 恢复不限额（余额清零，账本保留）
//	{"metered": true, "enforced": true} → 开启管控并在余额见底时拦截
//	{"metered": true, "enforced": false}→ 只计不拦（先观察一段时间再开闸）
func (s *Server) handleQuotaSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.store.GetUser(name); !ok {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	var req struct {
		Metered  bool `json:"metered"`
		Enforced bool `json:"enforced"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if !req.Metered {
		if err := s.store.RemoveQuota(name); err != nil {
			writeErr(w, http.StatusInternalServerError, "解除限额失败: "+err.Error())
			return
		}
		log.Printf("quota: %s 解除限额（操作人 %s）", name, reqUser(r).Name)
		writeJSON(w, http.StatusOK, quotaView{User: name})
		return
	}
	q, err := s.store.SetQuotaEnforced(name, req.Enforced)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	log.Printf("quota: %s 开启额度管控 enforced=%v（操作人 %s）", name, req.Enforced, reqUser(r).Name)
	writeJSON(w, http.StatusOK, viewQuota(name, q, true))
}

// maxGrantUSD 单次充值上限，纯粹是手滑护栏：多打几个零的代价比分两次充大得多。
const maxGrantUSD = 100_000

// handleCreditGrant 给用户充值（负数为冲正）。ref 是幂等键：同一个 ref 重复提交
// 只生效一次，前端重试或按钮连点都不会多扣多充。不带 ref 时服务端生成一个。
func (s *Server) handleCreditGrant(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.store.GetUser(name); !ok {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	var req struct {
		USD      float64 `json:"usd"`
		MicroUSD int64   `json:"micro_usd"` // 精确写法，非 0 时优先于 usd
		Note     string  `json:"note"`
		Ref      string  `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	delta := req.MicroUSD
	if delta == 0 {
		if math.IsNaN(req.USD) || math.IsInf(req.USD, 0) || math.Abs(req.USD) > maxGrantUSD {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("金额超出范围（上限 %d 美元）", maxGrantUSD))
			return
		}
		delta = int64(math.Round(req.USD * 1e6))
	}
	if delta == 0 {
		writeErr(w, http.StatusBadRequest, "充值金额不能为 0")
		return
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = "grant:" + store.NewID()
	} else {
		// 带上前缀，免得管理员随手填个 "usage:1" 撞掉一条消耗流水的幂等键。
		ref = "grant:" + ref
	}

	q, err := s.store.Grant(name, delta, ref, strings.TrimSpace(req.Note), reqUser(r).Name)
	switch {
	case err == store.ErrDuplicateRef:
		// 幂等命中：这次没变动，但余额是对的，按成功返回。
		writeJSON(w, http.StatusOK, map[string]any{
			"quota": viewQuota(name, q, true), "applied": false,
		})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "充值失败: "+err.Error())
		return
	}
	log.Printf("quota: %s %+d 微美元（ref=%s 操作人 %s）", name, delta, ref, reqUser(r).Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"quota": viewQuota(name, q, true), "applied": true,
	})
}

// handleUsageReport 按用户/时间段汇总消耗，给报表页用。
func (s *Server) handleUsageReport(w http.ResponseWriter, r *http.Request) {
	f := store.UsageFilter{
		User:      r.URL.Query().Get("user"),
		SessionID: r.URL.Query().Get("session"),
		Kind:      r.URL.Query().Get("kind"),
		Limit:     2000,
	}
	// 非管理员只能看自己的。
	if u := reqUser(r); u.Role != store.RoleAdmin {
		f.User = u.Name
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = ts
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			f.Until = ts
		}
	}
	events := s.store.ListUsage(f)

	// 汇总：按用户和模型各出一份，前端直接画表。duration 是回合级的，跨行
	// 求和没有意义，这里不汇总它。
	type bucket struct {
		Key          string `json:"key"`
		Turns        int    `json:"turns"`
		Input        int64  `json:"input_tokens"`
		Output       int64  `json:"output_tokens"`
		CacheRead    int64  `json:"cache_read_tokens"`
		CacheWrite   int64  `json:"cache_write_tokens"`
		CostMicroUSD int64  `json:"cost_micro_usd"`
	}
	byUser, byModel := map[string]*bucket{}, map[string]*bucket{}
	turns := map[string]map[string]bool{}
	var total bucket
	add := func(m map[string]*bucket, key string, e store.UsageEvent) {
		b := m[key]
		if b == nil {
			b = &bucket{Key: key}
			m[key] = b
		}
		b.Input += e.InputTokens
		b.Output += e.OutputTokens
		b.CacheRead += e.CacheReadTokens
		b.CacheWrite += e.CacheWriteTokens
		b.CostMicroUSD += e.CostMicroUSD
	}
	for _, e := range events {
		add(byUser, e.User, e)
		model := e.Model
		if model == "" {
			model = e.Agent + "(默认模型)"
		}
		add(byModel, model, e)
		total.Input += e.InputTokens
		total.Output += e.OutputTokens
		total.CacheRead += e.CacheReadTokens
		total.CacheWrite += e.CacheWriteTokens
		total.CostMicroUSD += e.CostMicroUSD
		// 一个回合可能拆成多行（claude 按模型分行），回合数按 turn_id 去重。
		if turns[e.User] == nil {
			turns[e.User] = map[string]bool{}
		}
		if !turns[e.User][e.TurnID] {
			turns[e.User][e.TurnID] = true
			byUser[e.User].Turns++
			total.Turns++
		}
	}
	flat := func(m map[string]*bucket) []bucket {
		out := make([]bucket, 0, len(m))
		for _, b := range m {
			out = append(out, *b)
		}
		// 花得多的排前面，报表第一眼就看见大头；同额按名字定序，免得每次刷新
		// 都换位置。
		sort.Slice(out, func(i, j int) bool {
			if out[i].CostMicroUSD != out[j].CostMicroUSD {
				return out[i].CostMicroUSD > out[j].CostMicroUSD
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":    total,
		"by_user":  flat(byUser),
		"by_model": flat(byModel),
		"rows":     len(events),
		// 触顶说明还有更早的数据没算进来，别把汇总当全量。
		"truncated": len(events) == f.Limit,
	})
}
