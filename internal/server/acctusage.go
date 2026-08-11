package server

import (
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
		writeErr(w, http.StatusNotFound, "会话绑定的账号已不存在")
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
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, claudeOAuthUsage, nil)
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "请求额度接口失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		writeErr(w, http.StatusBadGateway, "凭证已失效，请到「系统设置 → 账号」重新登录该账号")
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("额度接口返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200)))
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

// claudeCred 是账号池 .credentials.json 里 claudeAiOauth 的可用字段。
type claudeCred struct {
	AccessToken      string `json:"accessToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	SubscriptionType string `json:"subscriptionType"`
}

// readClaudeCred 从账号池取当前访问令牌与订阅档位。
//
// 只读不刷新：OAuth 刷新令牌是轮换制（见 credsync.go），服务端擅自刷新会作废
// 容器里 CLI 手上的那一份。令牌过期时如实报错，让用户跑一轮对话（CLI 会自己
// 刷新并由 credSync 回收）或重新登录。
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
	if exp := c.ClaudeAiOauth.ExpiresAt; exp > 0 && time.Now().UnixMilli() >= exp {
		return c.ClaudeAiOauth, fmt.Errorf("访问令牌已过期，发一轮对话让 CLI 自动续期后再查")
	}
	return c.ClaudeAiOauth, nil
}
