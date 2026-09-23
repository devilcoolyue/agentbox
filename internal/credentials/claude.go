package credentials

import (
	"encoding/json"
	"fmt"
	"time"

	"agentbox/internal/config"
)

type ClaudeCred struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	SubscriptionType string `json:"subscriptionType"`
}

// expiring 报告令牌是否已过期或即将过期。expiresAt 缺失（老凭证）时按「还能用」
// 处理：真失效了上游会回 401，那条路上有强制续期兜底。
func (c ClaudeCred) Expiring() bool {
	return c.ExpiresAt > 0 && time.Now().Add(credRefreshSkew).UnixMilli() >= c.ExpiresAt
}

// ReadClaude 从账号池读出当前 OAuth 凭证。
//
// 只管文件读不读得出来、字段齐不齐；到期与否交给 ensureClaudeCred 判断并自动
// 续期（见 credrefresh.go）。这里返回错误的三种情况都得人来管：账号没配
// credentials_dir、从没登录过、凭证被清空。
func ReadClaude(a config.Account) (ClaudeCred, error) {
	var c struct {
		ClaudeAiOauth ClaudeCred `json:"claudeAiOauth"`
	}
	if a.CredentialsDir == "" {
		return c.ClaudeAiOauth, fmt.Errorf("账号未配置 credentials_dir")
	}
	raw, err := readPoolFile(a, ".credentials.json")
	if err != nil {
		return c.ClaudeAiOauth, fmt.Errorf("账号还没登录，读不到凭证")
	}
	if json.Unmarshal(raw, &c) != nil || c.ClaudeAiOauth.AccessToken == "" {
		return c.ClaudeAiOauth, fmt.Errorf("凭证文件里没有访问令牌，请重新登录该账号")
	}
	return c.ClaudeAiOauth, nil
}
