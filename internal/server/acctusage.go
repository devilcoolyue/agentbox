package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"agentbox/internal/config"
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
	FetchedAt    string        `json:"fetched_at"`
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
	if acct.Type != config.AgentClaude {
		writeErr(w, http.StatusBadRequest, "只有 Claude 订阅账号能查额度")
		return
	}
	// 中转站账号打的是第三方 base_url，没有订阅额度这一说。
	if _, token := claudeRelay(acct); token != "" {
		writeErr(w, http.StatusBadRequest, "该账号走的是中转站 API Key，没有订阅额度可查")
		return
	}

	cred, err := readClaudeCred(acct)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	client, err := s.acctClient(acct, 20*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// 令牌过期就自己续，不把用户推回「先发一轮对话」的手动流程。
	if cred.expiring() {
		if cred, err = s.ensureClaudeCred(r.Context(), acct, false); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	raw, code, err := fetchOAuthUsage(r.Context(), client, cred.AccessToken)
	// 凭证里的 expiresAt 不是唯一真相：令牌可能被上游提前作废，也可能这份文件
	// 本身就落后于容器里那份。撞上 401 就强制续一次再打一遍。
	if code == http.StatusUnauthorized {
		if fresh, ferr := s.ensureClaudeCred(r.Context(), acct, true); ferr == nil {
			cred = fresh
			raw, code, err = fetchOAuthUsage(r.Context(), client, cred.AccessToken)
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "请求额度接口失败: "+err.Error())
		return
	}
	if code == http.StatusUnauthorized {
		writeErr(w, http.StatusBadGateway, "凭证已失效，请到「系统设置 → 账号」重新登录该账号")
		return
	}
	if code != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("额度接口返回 HTTP %d: %s", code, truncate(string(raw), 200)))
		return
	}

	var u oauthUsageResp
	if err := json.Unmarshal(raw, &u); err != nil {
		writeErr(w, http.StatusBadGateway, "额度响应解析失败")
		return
	}

	out := acctUsageView{
		AccountID:    acct.ID,
		AccountLabel: acct.Label,
		Plan:         cred.SubscriptionType,
		Windows:      []usageWindow{},
		FetchedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	for _, spec := range usageWindowLabels {
		win := spec.pick(&u)
		if win == nil || win.Utilization == nil {
			continue
		}
		out.Windows = append(out.Windows, usageWindow{
			Key:      spec.key,
			Label:    spec.label,
			Percent:  *win.Utilization,
			ResetsAt: win.ResetsAt,
		})
	}
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
		out.Extra = &x
	}

	writeJSON(w, http.StatusOK, out)
}

// fetchOAuthUsage 打一次额度接口，返回响应体与状态码。拆出来是因为 401 之后
// 要用续期出来的新令牌原样重打一遍。
func fetchOAuthUsage(ctx context.Context, client *http.Client, accessToken string) ([]byte, int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, claudeOAuthUsage, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// claudeCred 是账号池 .credentials.json 里 claudeAiOauth 的可用字段。
type claudeCred struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	SubscriptionType string `json:"subscriptionType"`
}

// expiring 报告令牌是否已过期或即将过期。expiresAt 缺失（老凭证）时按「还能用」
// 处理：真失效了上游会回 401，那条路上有强制续期兜底。
func (c claudeCred) expiring() bool {
	return c.ExpiresAt > 0 && time.Now().Add(credRefreshSkew).UnixMilli() >= c.ExpiresAt
}

// readClaudeCred 从账号池读出当前 OAuth 凭证。
//
// 只管文件读不读得出来、字段齐不齐；到期与否交给 ensureClaudeCred 判断并自动
// 续期（见 credrefresh.go）。这里返回错误的三种情况都得人来管：账号没配
// credentials_dir、从没登录过、凭证被清空。
func readClaudeCred(a config.Account) (claudeCred, error) {
	var c struct {
		ClaudeAiOauth claudeCred `json:"claudeAiOauth"`
	}
	if a.CredentialsDir == "" {
		return c.ClaudeAiOauth, fmt.Errorf("账号未配置 credentials_dir")
	}
	raw, err := os.ReadFile(filepath.Join(a.CredentialsDir, ".credentials.json"))
	if err != nil {
		return c.ClaudeAiOauth, fmt.Errorf("账号还没登录，读不到凭证")
	}
	if json.Unmarshal(raw, &c) != nil || c.ClaudeAiOauth.AccessToken == "" {
		return c.ClaudeAiOauth, fmt.Errorf("凭证文件里没有访问令牌，请重新登录该账号")
	}
	return c.ClaudeAiOauth, nil
}
