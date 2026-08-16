package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbox/internal/config"
)

// Claude OAuth 访问令牌的服务端自动续期。
//
// 令牌寿命只有几小时，而账号池里的这份是不是新鲜的，取决于容器里的 CLI 最近
// 有没有跑过——挂了一夜的账号，第二天点「查额度」必然是过期的。以前这里如实
// 报错让用户「先发一轮对话让 CLI 续一下」，现在服务端自己拿刷新令牌续。
//
// 之所以以前不敢续：刷新令牌是轮换制，一份用掉另一份就作废，服务端擅自刷新会
// 把容器里 CLI 手上那份变成废纸（下一轮对话直接掉登录）。解法不是不刷，而是
// 把刷新也纳入 credSync 那套「谁新用谁」的收敛里——刷之前先把各会话 home 里
// 可能更新的凭证收回池子，刷完立刻反向播发，全程对同一个账号串行。

// 令牌剩余寿命低于这个值就先续再用。取值刻意小：提前量越大，越容易和正在跑
// 的 CLI 抢同一个刷新令牌。一分钟只用来盖住「查到一半正好过期」。
const credRefreshSkew = time.Minute

// 每个账号一把锁：同一账号并发点「查额度」时只能有一次续期在飞，否则两个
// 请求各拿同一个刷新令牌去换，后到的那个必然 invalid_grant。
var credRefreshMu sync.Map // account id -> *sync.Mutex

func acctCredLock(id string) *sync.Mutex {
	v, _ := credRefreshMu.LoadOrStore(id, new(sync.Mutex))
	return v.(*sync.Mutex)
}

// oauthTokenResp 是 /v1/oauth/token 的响应，授权码换令牌与刷新续期共用。
// 订阅档位有时在顶层、有时在 account 里，两处都收。
type oauthTokenResp struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
	SubscriptionType      string `json:"subscription_type"`
	RateLimitTier         string `json:"rate_limit_tier"`
	Account               struct {
		SubscriptionType string `json:"subscription_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
	} `json:"account"`
}

// ensureClaudeCred 返回一份当下可用的访问令牌：池子里的还没到期就直接用，
// 过期（或 force）就用刷新令牌续一份，写回账号池并立刻播发到各会话容器。
//
// 顺序是有讲究的，三步都不能省：
//  1. 先跑一遍 credSync 把各会话 home 里可能更新的凭证收回池子——容器里的 CLI
//     刚续过的话，池子里这份已经是废的，拿它去刷只会白挨一个 invalid_grant，
//     而正确答案就躺在会话 home 里；
//  2. 收敛完再判断是否真的需要续；
//  3. 续完立刻反向播发，不等 credSyncLoop 那趟 45 秒的兜底——这段空窗里 CLI
//     手上还是老链条，一发对话就会拿作废的刷新令牌去换，换回一次要重新登录。
func (s *Server) ensureClaudeCred(ctx context.Context, acct config.Account, force bool) (claudeCred, error) {
	mu := acctCredLock(acct.ID)
	mu.Lock()
	defer mu.Unlock()

	s.syncAcctCreds(acct)

	cred, err := readClaudeCred(acct)
	if err != nil {
		return cred, err
	}
	if !force && !cred.expiring() {
		return cred, nil
	}
	if cred.RefreshToken == "" {
		return cred, fmt.Errorf("凭证里没有刷新令牌，请到「系统设置 → 账号」重新登录该账号")
	}

	fresh, err := s.refreshClaudeCred(ctx, acct, cred.RefreshToken)
	if err != nil {
		return cred, err
	}
	s.syncAcctCreds(acct)
	return fresh, nil
}

// refreshClaudeCred 用刷新令牌换一份新令牌并落回账号池。
func (s *Server) refreshClaudeCred(ctx context.Context, acct config.Account, refreshToken string) (claudeCred, error) {
	var out claudeCred
	// 必须走账号自己的出口 IP：续期来源与后续推理请求的 IP 对不上，正是订阅
	// 账号被判风控的典型形状（与登录换令牌同理，见 handleOAuthFinish）。
	client, err := s.acctClient(acct, 30*time.Second)
	if err != nil {
		return out, err
	}
	payload, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeOAuthClientID,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, claudeOAuthToken, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("令牌自动续期失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// 多半是 invalid_grant：刷新令牌被别处用掉或已吊销，只能重新登录。
		return out, fmt.Errorf("令牌自动续期失败 (HTTP %d)，请到「系统设置 → 账号」重新登录该账号: %s",
			resp.StatusCode, truncate(string(raw), 200))
	}
	var tok oauthTokenResp
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return out, fmt.Errorf("令牌自动续期响应解析失败")
	}
	return mergeClaudeCred(filepath.Join(acct.CredentialsDir, ".credentials.json"), tok)
}

// mergeClaudeCred 把新令牌并回凭证文件，返回并进去之后的那份。
//
// 刻意做「读旧的 → 改字段 → 写回」而不是整份重建：subscriptionType /
// rateLimitTier / scopes 这些是登录时才拿得到的，刷新响应里不一定回，覆盖没了
// 会被容器里的 Claude Code 当成 API 账号。同理，响应里没给的字段一律留原值。
func mergeClaudeCred(path string, tok oauthTokenResp) (claudeCred, error) {
	var out claudeCred
	root := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &root)
	}
	cur, _ := root["claudeAiOauth"].(map[string]any)
	if cur == nil {
		cur = map[string]any{}
	}

	now := time.Now().UnixMilli()
	cur["accessToken"] = tok.AccessToken
	if tok.RefreshToken != "" {
		cur["refreshToken"] = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		cur["expiresAt"] = now + tok.ExpiresIn*1000
	}
	if tok.RefreshTokenExpiresIn > 0 {
		cur["refreshTokenExpiresAt"] = now + tok.RefreshTokenExpiresIn*1000
	}
	if tok.Scope != "" {
		cur["scopes"] = strings.Fields(tok.Scope)
	}
	if v := firstNonEmpty(tok.SubscriptionType, tok.Account.SubscriptionType); v != "" {
		cur["subscriptionType"] = v
	}
	if v := firstNonEmpty(tok.RateLimitTier, tok.Account.RateLimitTier); v != "" {
		cur["rateLimitTier"] = v
	}
	root["claudeAiOauth"] = cur

	blob, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return out, fmt.Errorf("续期凭证序列化失败: %v", err)
	}
	// 原子替换：容器里的 CLI 随时可能在读同一个文件链上的副本，别让它读到半截。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return out, fmt.Errorf("写入续期凭证失败: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return out, fmt.Errorf("写入续期凭证失败: %v", err)
	}

	out.AccessToken = tok.AccessToken
	out.RefreshToken, _ = cur["refreshToken"].(string)
	out.SubscriptionType, _ = cur["subscriptionType"].(string)
	if v, ok := cur["expiresAt"].(int64); ok {
		out.ExpiresAt = v
	} else if v, ok := cur["expiresAt"].(float64); ok {
		out.ExpiresAt = int64(v) // 原样留用的旧值经 map 解码是 float64
	}
	return out, nil
}

// syncAcctCreds 让账号池与该账号名下所有会话 home 的轮换凭证就地收敛一次。
// credSyncLoop 每 45 秒做同样的事，这里只是不想等下一趟。
func (s *Server) syncAcctCreds(acct config.Account) {
	if acct.CredentialsDir == "" {
		return
	}
	for _, sess := range s.store.All() {
		if sess.AccountID == acct.ID {
			s.syncRotatingCred(acct, sess)
		}
	}
}
