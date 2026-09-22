package server

// 使用记录（流水明细）接口。`GET /api/usage` 出的是按用户/模型汇总的报表，
// 这里出的是一行行的原始消耗，给「使用记录」页翻页、筛选、导出用。
//
// 一行是什么，先说清楚，不然读数的人会按别处的直觉理解错：
//
//   一行 = 一个回合 × 一个模型，**不是一次 API 调用**。
//
// 容器里的 CLI 直接打 provider 官方接口，我们不在链路上，看不见单次 HTTP 请求；
// 拿得到的只有 CLI 在回合收尾汇总报的那一份账（见 usage.go）。一个回合内部
// claude 可能真打了十几次接口，这里合成一行。
//
// 行的来源有两种，靠 kind 区分：对话/起标题来自 runTurn 当场收到的收尾事件，
// 终端来自事后扫 CLI 自己的 transcript（见 termusage.go）。后者其实**看得见**
// 单次调用（transcript 里每条 assistant 都带 requestId 和最终 usage），但仍按
// 回合聚合落库，好让两种来源的行粒度一致、能放在一张表里比。
//
// 同理，中转站面板上的「端点 / API 密钥 / 分组」这几列我们没有对应物，别为了
// 凑齐列去编。

import (
	"net/http"
	"strconv"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// 一页多少行。前端可以调小，但不许调大：这个接口没有游标，全靠 OFFSET 翻页，
// 放开上限等于允许一次拖走整张表。
const (
	usageRowsDefault = 50
	usageRowsMax     = 500
	usageLocalLayout = "2006-01-02T15:04"
)

// 计费模式：这笔钱是怎么算出来的。对着一行说不清「为什么是这个数」的时候，
// 先看这个字段。
const (
	billingProvider = "provider" // provider 自己报的价（claude）
	billingTable    = "table"    // 按 config.json 的价目表折算（codex）
	billingNone     = "none"     // 没有价，只记不扣
)

// usageRowView 是明细表的一行。token 字段直接给数，金额一律微美元整数
// （前端只在显示的最后一步除 1e6，别在这儿转成浮点）。
type usageRowView struct {
	ID   int64  `json:"id"`
	TS   int64  `json:"ts"`
	User string `json:"user"`
	// SessionID/ThreadID 让前端能跳回产生这笔消耗的那个线程。SessionName 会在
	// 会话被删掉后变空，此时只剩 id 可看——这是正常的，不要因此隐藏该行。
	SessionName  string `json:"session_name,omitempty"`
	SessionID    string `json:"session_id"`
	ThreadID     string `json:"thread_id,omitempty"`
	TurnID       string `json:"turn_id"`
	Agent        string `json:"agent"`
	AccountID    string `json:"account_id,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	Model        string `json:"model,omitempty"`
	Kind         string `json:"kind"`
	Provider     string `json:"provider,omitempty"`
	Billing      string `json:"billing"`

	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CostMicroUSD     int64 `json:"cost_micro_usd"`
	DurationMS       int64 `json:"duration_ms"`
	WallMS           int64 `json:"wall_ms"`
	TTFTMs           int64 `json:"ttft_ms"`
	// Rate 让前端能把「输入 × 单价 + 输出 × 单价 + …」逐项摊开给用户看。
	// 查不到价时为 nil（那一行就是「未定价」）。
	Rate *usageRateView `json:"rate,omitempty"`
}

// usageRateView 交代一行的钱是按价目表里的哪一条、哪一档算的。
type usageRateView struct {
	// Key 是命中的价目表键：模型 ID，或作为兜底的 agent 名。
	Key string `json:"key"`
	// Basis 区分这份单价的分量：
	//   table     —— 这一行的钱就是它算出来的；
	//   reference —— provider 自报了总额，这份单价只是照价目表推的参考拆分，
	//                逐项加起来不一定等于实收金额。
	Basis string `json:"basis"`
	// Long 为真表示这个回合的输入超过阈值、整体走了长上下文档的单价。
	Long bool  `json:"long,omitempty"`
	Over int64 `json:"long_context_over,omitempty"`
	// 四个桶的单价，美元 / 百万 token（已按上面的档位选好）。
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

const (
	rateBasisTable     = "table"
	rateBasisReference = "reference"
)

// tableBilled 说明这一行的钱该不该由价目表算。claude 的对话行以 provider 自报
// 的为准；终端行是从 transcript 补记的，那里只有 token 没有美元——哪怕是
// claude，价也只能查表。
func tableBilled(e store.UsageEvent) bool {
	return e.Kind == store.UsageKindTerminal || e.Agent != config.AgentClaude
}

// billingMode 说明这一行的 cost 是哪来的。claude 的对话行永远以 provider 报的
// 为准，哪怕报的是 0（子 agent 偶尔如此）；查表的那些有价就是查表来的。
func billingMode(e store.UsageEvent) string {
	if !tableBilled(e) {
		return billingProvider
	}
	if e.CostMicroUSD != 0 {
		return billingTable
	}
	return billingNone
}

// rateFor 取这一行在**当前**价目表下的单价。历史行的实收金额是记账当时算的，
// 中途改过价就对不上——差多少由前端自己比对后提示，这里不猜。
func (s *Server) rateFor(e store.UsageEvent) *usageRateView {
	p, key, ok := s.cfg.PriceLookup(e.Agent, e.Model)
	if !ok {
		return nil
	}
	// 档位判定必须跟 priceEvent 用同一个口径：未命中缓存的输入 + 命中缓存的输入。
	prompt := e.InputTokens + e.CacheReadTokens
	r := p.Rates(prompt)
	v := &usageRateView{
		Key: key, Basis: rateBasisReference,
		Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
	}
	if tableBilled(e) {
		v.Basis = rateBasisTable
	}
	if p.Long != nil && p.LongContextOver > 0 && prompt > p.LongContextOver {
		v.Long = true
		v.Over = p.LongContextOver
	}
	return v
}

// usageFilterFrom 从查询串解析过滤条件，并把可见范围钉死。
//
// 返回的 scopeUser 是这个请求的硬边界：普通用户只能是自己，管理员为空（看全部）。
// 它与 f.User 是两回事——f.User 是用户自己选的筛选项，会被 facets 放宽，
// scopeUser 不会。
func (s *Server) usageFilterFrom(r *http.Request) (f store.UsageFilter, scopeUser string) {
	q := r.URL.Query()
	f = store.UsageFilter{
		User:      q.Get("user"),
		SessionID: q.Get("session"),
		Kind:      q.Get("kind"),
		Agent:     q.Get("agent"),
		Model:     q.Get("model"),
	}
	if u := reqUser(r); u.Role != store.RoleAdmin {
		scopeUser = u.Name
		f.User = u.Name // 无视传进来的 user 参数，别人的流水一行都不给
	}
	loc := s.cfg.GetLocation()
	if v := q.Get("since"); v != "" {
		if ts, err := parseUsageTime(v, loc); err == nil {
			f.Since = ts
		}
	}
	if v := q.Get("until"); v != "" {
		if ts, err := parseUsageTime(v, loc); err == nil {
			f.Until = ts
		}
	}
	// 排序只认时间一列（前端表头那对上下箭头），默认最新在最前——看消耗基本都是
	// 先看刚花掉的那笔；order=asc 翻过来，用来从头核对一段时间的账。
	// 别的列不开放排序：offset 翻页要求排序键在两次请求之间稳定，而金额/token
	// 这些列会随「补记终端消耗」这类后台写入变动。
	f.Asc = q.Get("order") == "asc"
	return f, scopeUser
}

// parseUsageTime accepts RFC3339 for compatibility with older clients and a
// timezone-less wall-clock value for the current UI. The latter is interpreted
// in the system timezone, so a filter means the same thing on every browser.
func parseUsageTime(v string, loc *time.Location) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339, v); err == nil {
		return ts, nil
	}
	return time.ParseInLocation(usageLocalLayout, v, loc)
}

// handleUsageEvents 返回一页用量明细，外加整个筛选范围的合计与可选项。
//
// 合计与可选项都按「筛选条件」算而不是按「当前这一页」算：翻到第 3 页时表头
// 显示的仍是整个筛选结果的总花费，否则每翻一页数字都变，没法用。
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	// 先补一趟终端消耗再查，否则刚在终端里花掉的量要等下一次定时扫描才出现，
	// 用户看到的是「我刚用完，表里没有」。没改动过的 transcript 只花一次 stat，
	// 真正要读的只有刚写过的那个文件。
	s.scanTerminalUsage(s.termScan)

	f, scopeUser := s.usageFilterFrom(r)
	q := r.URL.Query()

	f.Limit = usageRowsDefault
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		f.Limit = min(v, usageRowsMax)
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v > 0 {
		f.Offset = v
	}

	// 会话名按 id 查一次就够：一页里同一个会话通常占好几行。
	names := map[string]string{}
	sessionName := func(id string) string {
		if id == "" {
			return ""
		}
		if n, ok := names[id]; ok {
			return n
		}
		n := ""
		if sess, ok := s.store.Get(id); ok {
			n = sess.Name
		}
		names[id] = n
		return n
	}

	rows := []usageRowView{}
	for _, e := range s.store.ListUsage(f) {
		label := ""
		if acct, ok := s.cfg.Account(e.AccountID); ok {
			label = acct.Label
		}
		rows = append(rows, usageRowView{
			ID: e.ID, TS: e.TS.UnixMilli(), User: e.User,
			SessionName: sessionName(e.SessionID), SessionID: e.SessionID,
			ThreadID: e.ThreadID, TurnID: e.TurnID,
			Agent: e.Agent, AccountID: e.AccountID, AccountLabel: label,
			Model: e.Model, Kind: e.Kind, Provider: e.Provider,
			Billing:          billingMode(e),
			InputTokens:      e.InputTokens,
			OutputTokens:     e.OutputTokens,
			CacheReadTokens:  e.CacheReadTokens,
			CacheWriteTokens: e.CacheWriteTokens,
			TotalTokens:      e.InputTokens + e.OutputTokens + e.CacheReadTokens + e.CacheWriteTokens,
			CostMicroUSD:     e.CostMicroUSD,
			DurationMS:       e.DurationMS,
			WallMS:           e.WallMS,
			TTFTMs:           e.TTFTMs,
			Rate:             s.rateFor(e),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rows":   rows,
		"total":  s.store.SumUsage(f),
		"facets": s.store.FacetsUsage(f, scopeUser),
		"limit":  f.Limit,
		"offset": f.Offset,
		// 回声一份当前排序方向，前端据此点亮表头上那个箭头——刷新或从别处跳
		// 回来时，箭头显示的方向不会和实际拿到的顺序对不上。
		"order": map[bool]string{true: "asc", false: "desc"}[f.Asc],
		// 前端据此决定要不要显示「用户」列和用户筛选框：只看得到自己的时候
		// 那一列每行都一样，纯占地方。
		"scope":    map[bool]string{true: "self", false: "all"}[scopeUser != ""],
		"timezone": s.cfg.GetTimeZone(),
	})
}
