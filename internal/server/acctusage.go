package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
	"agentbox/internal/store"
)

// Claude 订阅额度查询接口。官方 CLI 的 /usage 打的就是它：拿 OAuth 访问令牌
// 换回 5 小时窗口 + 周窗口的使用百分比。需要 oauth beta 头，否则 401。
const claudeOAuthBeta = "oauth-2025-04-20"

// 变量而非常量：测试里指向 httptest 假上游。
var claudeOAuthUsage = "https://api.anthropic.com/api/oauth/usage"

// usageWindow 是一条归一化后的限额窗口。Percent 为 0-100 的使用率，
// ResetsAt 是 RFC3339 时间戳（部分窗口不下发，留空表示未知）。
type usageWindow struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at,omitempty"`
}

// usageExtra 是订阅额度用尽后继续走的「额外用量」信用金，按货币金额计。
type usageExtra struct {
	Enabled  bool    `json:"enabled"`
	Percent  float64 `json:"percent"`
	Used     float64 `json:"used"`
	Limit    float64 `json:"limit"`
	Currency string  `json:"currency,omitempty"`
}

// acctUsageView 是 /api/sessions/{id}/account/usage 的响应。
type acctUsageView struct {
	AccountID    string        `json:"account_id"`
	AccountLabel string        `json:"account_label"`
	Plan         string        `json:"plan,omitempty"`
	Windows      []usageWindow `json:"windows"`
	Extra        *usageExtra   `json:"extra,omitempty"`
	// FetchedAt 是数据的时间：对话响应最近一次报的时间，或官方接口查到的时间。
	FetchedAt string `json:"fetched_at,omitempty"`
	// Source 说明较新的那份数据从哪来：responses（对话响应附带）或 upstream
	// （官方额度接口）。
	Source string `json:"source,omitempty"`
	// ThrottledUntil：官方接口在 429 冷却中，到这个时间前刷新不会去打它。
	ThrottledUntil string `json:"throttled_until,omitempty"`
	// RefreshError：这次刷新官方接口失败了（429 以外），返回的是已有数据。
	RefreshError string `json:"refresh_error,omitempty"`
}

// 上游返回的窗口结构。utilization 是 0-100 的浮点，账号没有该窗口时整个
// 对象为 null（如 pro 账号没有按模型细分的周窗口）。
type oauthUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type oauthUsageResp struct {
	FiveHour       *oauthUsageWindow `json:"five_hour"`
	SevenDay       *oauthUsageWindow `json:"seven_day"`
	SevenDayOpus   *oauthUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *oauthUsageWindow `json:"seven_day_sonnet"`
	ExtraUsage     *struct {
		IsEnabled     bool     `json:"is_enabled"`
		MonthlyLimit  *float64 `json:"monthly_limit"`
		UsedCredits   *float64 `json:"used_credits"`
		Utilization   *float64 `json:"utilization"`
		Currency      string   `json:"currency"`
		DecimalPlaces *int     `json:"decimal_places"`
	} `json:"extra_usage"`
}

// 只挑官方 /usage 会展示的四个窗口。响应里还有一批内部代号窗口
// （tangelo / nimbus_quill / omelette 之类），对用户没有意义，不透出。
var usageWindowLabels = []struct {
	key   string
	label string
	pick  func(*oauthUsageResp) *oauthUsageWindow
}{
	{"five_hour", "5 小时", func(r *oauthUsageResp) *oauthUsageWindow { return r.FiveHour }},
	{"seven_day", "本周", func(r *oauthUsageResp) *oauthUsageWindow { return r.SevenDay }},
	{"seven_day_opus", "本周 Opus", func(r *oauthUsageResp) *oauthUsageWindow { return r.SevenDayOpus }},
	{"seven_day_sonnet", "本周 Sonnet", func(r *oauthUsageResp) *oauthUsageWindow { return r.SevenDaySonnet }},
}

// handleAccountUsage 查该会话所用 Claude 账号的订阅额度。
//
// 走会话维度而不是账号维度：普通用户看不到账号管理接口，但看得见自己会话绑
// 的是哪个账号，鉴权也就自然跟着会话归属走。
func (s *Server) handleAccountUsage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	acct, ok := s.cfg.Account(sess.AccountID)
	if !ok {
		writeErr(w, http.StatusNotFound, "工作空间绑定的账号已不存在")
		return
	}
	if !s.canUseAccount(acct, sess.User) {
		writeErr(w, http.StatusForbidden, errAccountAccess.Error())
		return
	}
	if acct.Type != config.AgentClaude {
		writeErr(w, http.StatusBadRequest, "只有 Claude 订阅账号能查额度")
		return
	}
	// 中转站账号打的是第三方 base_url，没有订阅额度这一说。
	if _, token := claudeRelay(acct); token != "" {
		writeErr(w, http.StatusBadRequest, "该账号走的是中转站 API Key，没有订阅额度可查")
		return
	}

	cred, err := credentials.ReadClaude(acct)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 默认只用手上的数据（对话响应附带的窗口、上次查官方接口的结果）；
	// 用户点刷新、或者什么都没有时才去打官方接口。
	refresh := r.URL.Query().Get("refresh") == "1"
	st := s.acctUsage.state(acct.ID)
	_, _, _, _, have := mergeUsage(st, time.Now())
	var fetchErr error
	if refresh || !have {
		fetchErr = s.acctUsage.fetchUpstream(acct.ID, time.Now, func() upstreamResult {
			// 发起刷新的请求断开了也让这次查询做完，同账号在等它的请求要用。
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
			defer cancel()
			return s.queryOAuthUsage(ctx, acct, cred)
		})
		st = s.acctUsage.state(acct.ID)
	}

	out := acctUsageView{
		AccountID:    acct.ID,
		AccountLabel: acct.Label,
		Plan:         cred.SubscriptionType,
		Windows:      []usageWindow{},
	}
	if until := st.upstream.blockedUntil; time.Now().Before(until) {
		out.ThrottledUntil = until.UTC().Format(time.RFC3339)
	}
	windows, extra, source, at, ok := mergeUsage(st, time.Now())
	var throttled errUsageThrottled
	switch {
	case ok:
		out.Windows, out.Extra, out.Source = windows, extra, source
		out.FetchedAt = at.UTC().Format(time.RFC3339)
		if fetchErr != nil && !errors.As(fetchErr, &throttled) {
			out.RefreshError = fetchErr.Error()
		}
	case errors.As(fetchErr, &throttled):
		// 什么数据都没有、官方接口又在冷却：照样 200，让页面说明什么时候能再查。
	case fetchErr != nil:
		code := http.StatusBadGateway
		var credErr usageCredError
		if errors.As(fetchErr, &credErr) {
			code = http.StatusBadRequest // 凭证本身的问题，要人去处理，不是上游故障
		}
		writeErr(w, code, fetchErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// queryOAuthUsage 打一次官方额度接口并解析，令牌快过期或被上游作废时先续期。
func (s *Server) queryOAuthUsage(ctx context.Context, acct config.Account, cred credentials.ClaudeCred) upstreamResult {
	client, err := s.acctClient(acct, 20*time.Second)
	if err != nil {
		return upstreamResult{err: err}
	}
	// 令牌过期就自己续，不把用户推回「先发一轮对话」的手动流程。
	if cred.Expiring() {
		if cred, err = s.ensureClaudeCred(ctx, acct, false); err != nil {
			return upstreamResult{err: usageCredError{err}}
		}
	}

	raw, code, header, err := fetchOAuthUsage(ctx, client, cred.AccessToken)
	// 凭证里的 expiresAt 不是唯一真相：令牌可能被上游提前作废，也可能这份文件
	// 本身就落后于容器里那份。撞上 401 就强制续一次再打一遍。
	if code == http.StatusUnauthorized {
		if fresh, ferr := s.ensureClaudeCred(ctx, acct, true); ferr == nil {
			raw, code, header, err = fetchOAuthUsage(ctx, client, fresh.AccessToken)
		}
	}
	switch {
	case err != nil:
		return upstreamResult{err: errors.New("请求额度接口失败: " + err.Error())}
	case code == http.StatusUnauthorized:
		return upstreamResult{err: errors.New("凭证已失效，请到「系统设置 → 账号」重新登录该账号")}
	case code == http.StatusTooManyRequests:
		return upstreamResult{retryAfter: usageRetryAfter(header, time.Now())}
	case code != http.StatusOK:
		return upstreamResult{err: fmt.Errorf("额度接口返回 HTTP %d: %s", code, truncate(string(raw), 200))}
	}
	windows, extra, err := parseOAuthUsage(raw)
	if err != nil {
		return upstreamResult{err: err}
	}
	return upstreamResult{windows: windows, extra: extra}
}

// usageCredError 是续期凭证失败：要人去重新登录，而不是上游的临时故障。
type usageCredError struct{ err error }

func (e usageCredError) Error() string { return e.err.Error() }
func (e usageCredError) Unwrap() error { return e.err }

// parseOAuthUsage 把官方额度接口的响应归一成窗口列表与额外用量。
func parseOAuthUsage(raw []byte) ([]usageWindow, *usageExtra, error) {
	var u oauthUsageResp
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, nil, errors.New("额度响应解析失败")
	}
	windows := []usageWindow{}
	for _, spec := range usageWindowLabels {
		win := spec.pick(&u)
		if win == nil || win.Utilization == nil {
			continue
		}
		windows = append(windows, usageWindow{
			Key:      spec.key,
			Label:    spec.label,
			Percent:  *win.Utilization,
			ResetsAt: win.ResetsAt,
		})
	}
	var extra *usageExtra
	if e := u.ExtraUsage; e != nil && e.IsEnabled {
		// 金额按 decimal_places 缩放：monthly_limit=3500 + decimal_places=2 是 $35.00。
		scale := 1.0
		if e.DecimalPlaces != nil && *e.DecimalPlaces > 0 {
			for i := 0; i < *e.DecimalPlaces; i++ {
				scale *= 10
			}
		}
		x := usageExtra{Enabled: true, Currency: e.Currency}
		if e.Utilization != nil {
			x.Percent = *e.Utilization
		}
		if e.UsedCredits != nil {
			x.Used = *e.UsedCredits / scale
		}
		if e.MonthlyLimit != nil {
			x.Limit = *e.MonthlyLimit / scale
		}
		extra = &x
	}
	return windows, extra, nil
}

// fetchOAuthUsage 打一次额度接口，返回响应体与状态码。拆出来是因为 401 之后
// 要用续期出来的新令牌原样重打一遍。
func fetchOAuthUsage(ctx context.Context, client *http.Client, accessToken string) ([]byte, int, http.Header, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, claudeOAuthUsage, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, resp.Header, nil
}

// claudeCred 是账号池 .credentials.json 里 claudeAiOauth 的可用字段。
