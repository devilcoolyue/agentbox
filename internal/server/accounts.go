package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
)

// Claude 订阅登录的 OAuth 常量。全部提取自本机安装的 Claude Code 二进制
// （strings claude.exe），与官方 CLI 的授权流程保持一致：
// claude.ai 订阅授权入口 + PKCE(S256) + platform.claude.com 换令牌。
const (
	claudeOAuthClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthAuthorize = "https://claude.com/cai/oauth/authorize"
	claudeOAuthRedirect  = "https://platform.claude.com/oauth/code/callback"
	claudeOAuthProfile   = "https://api.anthropic.com/api/oauth/profile"
	claudeOAuthScope     = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	oauthPendingTTL      = 15 * time.Minute
)

// 变量而非常量：测试里指向 httptest 假上游。授权码换令牌和刷新令牌续期
// （credrefresh.go）打的是同一个端点。
var claudeOAuthToken = "https://platform.claude.com/v1/oauth/token"

// oauthPending 是一次进行中的授权（start 已发、等待用户贴回授权码）。
type oauthPending struct {
	verifier string
	state    string
	created  time.Time
	busy     bool
}

var (
	oauthMu   sync.Mutex
	oauthPend = map[string]oauthPending{} // account id -> pending
)

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// handleOAuthStart 生成授权链接。用户在浏览器完成授权后，把回显的授权码
// 贴回 finish 接口。
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if acct.Type != config.AgentClaude && acct.Type != config.AgentCodex {
		writeErr(w, http.StatusBadRequest, "该账号类型不使用 OAuth 登录")
		return
	}
	if acct.CredentialsDir == "" {
		writeErr(w, http.StatusBadRequest, "账号未配置 credentials_dir")
		return
	}

	verifier := randB64(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randB64(32)

	oauthMu.Lock()
	if oauthPend[acct.ID].busy {
		oauthMu.Unlock()
		writeErr(w, http.StatusConflict, "正在完成授权，请稍后重试")
		return
	}
	for id, pending := range oauthPend {
		if time.Since(pending.created) > oauthPendingTTL && !pending.busy {
			delete(oauthPend, id)
		}
	}
	oauthPend[acct.ID] = oauthPending{verifier: verifier, state: state, created: time.Now()}
	oauthMu.Unlock()

	q := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {claudeOAuthRedirect},
		"scope":                 {claudeOAuthScope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	authorize := claudeOAuthAuthorize
	if acct.Type == config.AgentCodex {
		authorize = codexOAuthAuthorize
		q.Del("code")
		q.Set("client_id", codexOAuthClientID)
		q.Set("redirect_uri", codexOAuthRedirect)
		q.Set("scope", "openid profile email offline_access")
		q.Set("id_token_add_organizations", "true")
		q.Set("codex_cli_simplified_flow", "true")
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": authorize + "?" + q.Encode()})
}

// handleOAuthFinish 用授权码换令牌并写入账号池。写入后 credsync 会把新
// 凭证播发到各会话容器。
func (s *Server) handleOAuthFinish(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if acct.Type == config.AgentCodex {
		s.handleCodexOAuthFinish(w, r, acct)
		return
	}
	if acct.Type != config.AgentClaude {
		writeErr(w, http.StatusBadRequest, "该账号类型不使用 OAuth 登录")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Code) == "" {
		writeErr(w, http.StatusBadRequest, "缺少授权码")
		return
	}

	oauthMu.Lock()
	pend, ok := oauthPend[acct.ID]
	oauthMu.Unlock()
	if !ok || time.Since(pend.created) > oauthPendingTTL {
		writeErr(w, http.StatusBadRequest, "授权已过期，请重新生成授权链接")
		return
	}

	// 授权页回显格式为 <code>#<state>
	code := strings.TrimSpace(body.Code)
	state := pend.state
	if i := strings.IndexByte(code, '#'); i >= 0 {
		if code[i+1:] != pend.state {
			writeErr(w, http.StatusBadRequest, "state 不匹配，请重新生成授权链接")
			return
		}
		code = code[:i]
	}

	payload, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     claudeOAuthClientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  claudeOAuthRedirect,
		"code_verifier": pend.verifier,
	})
	// 换令牌和随后的 profile 查询都必须走账号自己的出口 IP：登录来源与后续
	// 推理请求的 IP 对不上，正是订阅账号被判风控的典型形状。
	client, err := s.acctClient(acct, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, claudeOAuthToken, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "请求令牌接口失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("换取令牌失败 (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 300)))
		return
	}

	var tok credentials.OAuthTokenResponse
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		writeErr(w, http.StatusBadGateway, "令牌响应解析失败")
		return
	}

	// 订阅类型/限速档位不在令牌响应里，官方 CLI 是换完令牌后再调 profile
	// 接口补查的。凭证里缺 subscriptionType 会被 Claude Code 当成 API 账号。
	subType := firstNonEmpty(tok.SubscriptionType, tok.Account.SubscriptionType)
	rateTier := firstNonEmpty(tok.RateLimitTier, tok.Account.RateLimitTier)
	if subType == "" || rateTier == "" {
		pSub, pTier := fetchOAuthProfile(r.Context(), client, tok.AccessToken)
		subType = firstNonEmpty(subType, pSub)
		rateTier = firstNonEmpty(rateTier, pTier)
	}

	now := time.Now().UnixMilli()
	cred := map[string]any{
		"accessToken":  tok.AccessToken,
		"refreshToken": tok.RefreshToken,
		"expiresAt":    now + tok.ExpiresIn*1000,
	}
	if tok.RefreshTokenExpiresIn > 0 {
		cred["refreshTokenExpiresAt"] = now + tok.RefreshTokenExpiresIn*1000
	}
	if tok.Scope != "" {
		cred["scopes"] = strings.Fields(tok.Scope)
	} else {
		cred["scopes"] = strings.Fields(claudeOAuthScope)
	}
	if subType != "" {
		cred["subscriptionType"] = subType
	}
	if rateTier != "" {
		cred["rateLimitTier"] = rateTier
	}
	out, _ := json.MarshalIndent(map[string]any{"claudeAiOauth": cred}, "", "  ")

	if err := s.credentialService().SaveClaude(r.Context(), acct.ID, out); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	oauthMu.Lock()
	delete(oauthPend, acct.ID)
	oauthMu.Unlock()

	// 中转站 env 优先于 OAuth 凭证，不清掉的话这次登录形同虚设
	if err := s.clearClaudeRelay(acct); err != nil {
		writeErr(w, http.StatusInternalServerError, "登录成功但清除中转站配置失败: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"subscription_type": subType,
		"expires_at":        now + tok.ExpiresIn*1000,
	})
}

// fetchOAuthProfile 用新令牌查订阅身份，返回 subscriptionType（pro/max/...）
// 和 rateLimitTier。client 由调用方按账号绑定的代理构造。失败不致命，只影响
// 容器内 /status 显示的登录方式。
func fetchOAuthProfile(ctx context.Context, client *http.Client, accessToken string) (subType, rateTier string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, claudeOAuthProfile, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	var p struct {
		Account struct {
			HasClaudeMax bool `json:"has_claude_max"`
			HasClaudePro bool `json:"has_claude_pro"`
		} `json:"account"`
		Organization struct {
			OrganizationType string `json:"organization_type"` // claude_pro / claude_max / ...
			RateLimitTier    string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p) != nil {
		return "", ""
	}
	switch {
	case p.Account.HasClaudeMax:
		subType = "max"
	case p.Account.HasClaudePro:
		subType = "pro"
	case strings.HasPrefix(p.Organization.OrganizationType, "claude_"):
		subType = strings.TrimPrefix(p.Organization.OrganizationType, "claude_")
	}
	return subType, p.Organization.RateLimitTier
}

// claude 走中转站的标准环境变量。实测（假凭证 + 本地假 API 抓包）：这两个
// env 存在时 claude CLI 直接用 Bearer <token> 打 base_url，优先于同目录下的
// OAuth .credentials.json——所以切换到中转站不必清理订阅凭证，反向切换
// （OAuth 登录）则必须清掉这两个 env，否则订阅登录形同虚设。
const (
	envAnthropicBaseURL = "ANTHROPIC_BASE_URL"
	envAnthropicToken   = "ANTHROPIC_AUTH_TOKEN"
	envAnthropicAPIKey  = "ANTHROPIC_API_KEY"
)

// claudeRelay 返回 claude 账号 env 里的中转站配置；token 为空即订阅模式。
func claudeRelay(a config.Account) (baseURL, token string) {
	if t := a.Env[envAnthropicToken]; t != "" {
		return a.Env[envAnthropicBaseURL], t
	}
	if t := a.Env[envAnthropicAPIKey]; t != "" {
		return a.Env[envAnthropicBaseURL], t
	}
	return "", ""
}

// clearClaudeRelay 从账号 env 里摘掉中转站键并落盘；无键时不写盘。
func (s *Server) clearClaudeRelay(acct config.Account) error {
	_, token := claudeRelay(acct)
	if token == "" && acct.Env[envAnthropicBaseURL] == "" {
		return nil
	}
	env := map[string]string{}
	for k, v := range acct.Env {
		if k != envAnthropicBaseURL && k != envAnthropicToken && k != envAnthropicAPIKey {
			env[k] = v
		}
	}
	_, err := s.cfg.UpdateAccount(acct.ID, config.AccountPatch{Env: &env})
	return err
}

// handleSetAPIKey 保存中转站/官方 API Key。
//   - codex：Key 写 auth.json，base_url/wire_api 写 config.toml（codex 只认
//     config.toml 里的 model_provider 指向，缺了它 key 再对也会打到官方 401）
//   - claude：写账号 env（ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN），随
//     exec 注入容器，对新对话/新终端立即生效
func (s *Server) handleSetAPIKey(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body struct {
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url"`
		WireAPI string `json:"wire_api"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" || strings.ContainsAny(key, "\r\n\"\\") || len(key) > 512 {
		writeErr(w, http.StatusBadRequest, "API Key 为空或含非法字符")
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if baseURL != "" {
		if err := validateBaseURL(baseURL); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	switch acct.Type {
	case config.AgentClaude:
		env := map[string]string{}
		for k, v := range acct.Env {
			env[k] = v
		}
		env[envAnthropicToken] = key
		delete(env, envAnthropicAPIKey) // 统一走 AUTH_TOKEN，避免双键歧义
		if baseURL != "" {
			env[envAnthropicBaseURL] = baseURL
		} else {
			delete(env, envAnthropicBaseURL) // 留空 = 官方 api.anthropic.com
		}
		if _, err := s.cfg.UpdateAccount(acct.ID, config.AccountPatch{Env: &env}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	case config.AgentCodex:
		if acct.CredentialsDir == "" {
			writeErr(w, http.StatusBadRequest, "账号未配置 credentials_dir")
			return
		}
		wireAPI := body.WireAPI
		if wireAPI == "" {
			wireAPI = "responses"
		}
		if wireAPI != "responses" && wireAPI != "chat" {
			writeErr(w, http.StatusBadRequest, "wire_api 只能是 responses 或 chat")
			return
		}
		if err := s.credentialService().SaveCodexKey(r.Context(), acct.ID, key, baseURL, wireAPI); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "该账号类型不使用 API Key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleClearAPIKey 清除 claude 账号的中转站配置，切回订阅 OAuth 凭证。
func (s *Server) handleClearAPIKey(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if acct.Type != config.AgentClaude {
		writeErr(w, http.StatusBadRequest, "仅 claude 账号支持清除中转站配置")
		return
	}
	if err := s.clearClaudeRelay(acct); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func validateBaseURL(raw string) error {
	if strings.ContainsAny(raw, " \t\r\n\"\\") || len(raw) > 512 {
		return fmt.Errorf("Base URL 含非法字符")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("Base URL 必须是 http(s)://host[/path] 形式")
	}
	return nil
}

// handleAPIKeyTest 探测中转站连通性：GET {base}/v1/models 拉模型列表，能
// 拉到即认为 key+base_url 可用。字段留空时回退到账号池里已保存的值。
func (s *Server) handleAPIKeyTest(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body struct {
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	key := strings.TrimSpace(body.APIKey)
	baseURL := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	switch acct.Type {
	case config.AgentClaude:
		if key == "" {
			_, key = claudeRelay(acct)
		}
		if baseURL == "" {
			baseURL, _ = claudeRelay(acct)
		}
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
	case config.AgentCodex:
		if key == "" {
			key = credentials.SavedAPIKey(acct.CredentialsDir)
		}
		if baseURL == "" {
			baseURL, _ = credentials.ReadCodexProvider(acct.CredentialsDir)
		}
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
	default:
		writeErr(w, http.StatusBadRequest, "该账号类型不使用 API Key")
		return
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, "请先输入 API Key（或保存过一次后可留空）")
		return
	}
	if err := validateBaseURL(baseURL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 探测也走账号绑定的出口 IP，否则「这里能通」跟容器里能不能通是两回事。
	client, err := s.acctClient(acct, 20*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer client.CloseIdleConnections()
	found, err := listProviderModels(r.Context(), client, acct.Type, baseURL, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+key)
		if acct.Type == config.AgentClaude {
			// 官方 /v1/models 认 x-api-key + anthropic-version，中转站认 Bearer
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "连接失败: "+err.Error())
		return
	}
	models := make([]string, 0, len(found.Models))
	for _, m := range found.Models {
		models = append(models, m.ID)
	}
	sort.Strings(models)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"endpoint":   found.Endpoint,
		"latency_ms": found.LatencyMS,
		"models":     models,
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
