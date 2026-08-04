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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"agentbox/internal/config"
)

// Claude 订阅登录的 OAuth 常量。全部提取自本机安装的 Claude Code 二进制
// （strings claude.exe），与官方 CLI 的授权流程保持一致：
// claude.ai 订阅授权入口 + PKCE(S256) + platform.claude.com 换令牌。
const (
	claudeOAuthClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthAuthorize = "https://claude.com/cai/oauth/authorize"
	claudeOAuthToken     = "https://platform.claude.com/v1/oauth/token"
	claudeOAuthRedirect  = "https://platform.claude.com/oauth/code/callback"
	claudeOAuthProfile   = "https://api.anthropic.com/api/oauth/profile"
	claudeOAuthScope     = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	oauthPendingTTL      = 15 * time.Minute
)

// oauthPending 是一次进行中的授权（start 已发、等待用户贴回授权码）。
type oauthPending struct {
	verifier string
	state    string
	created  time.Time
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
	if acct.Type != config.AgentClaude {
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
	writeJSON(w, http.StatusOK, map[string]string{"url": claudeOAuthAuthorize + "?" + q.Encode()})
}

// handleOAuthFinish 用授权码换令牌并写入账号池。写入后 credsync 会把新
// 凭证播发到各会话容器。
func (s *Server) handleOAuthFinish(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
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

	var tok struct {
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

	dst := filepath.Join(acct.CredentialsDir, ".credentials.json")
	if err := os.MkdirAll(acct.CredentialsDir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(dst, out, 0o600); err != nil {
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
		if err := os.MkdirAll(acct.CredentialsDir, 0o700); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, _ := json.MarshalIndent(map[string]string{"OPENAI_API_KEY": key}, "", "  ")
		dst := filepath.Join(acct.CredentialsDir, "auth.json")
		if err := os.WriteFile(dst, out, 0o600); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// base_url 留空表示走官方接口/保持现有 config.toml 不动
		if baseURL != "" {
			if err := writeCodexProviderTOML(acct.CredentialsDir, baseURL, wireAPI); err != nil {
				writeErr(w, http.StatusInternalServerError, "config.toml 写入失败: "+err.Error())
				return
			}
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

// config.toml 里 provider 相关行的定位正则。只处理简单双引号字符串，
// 手写复杂 TOML（多行字符串等）不在覆盖范围。
var (
	reTomlBaseURL  = regexp.MustCompile(`(?m)^(\s*base_url\s*=\s*)"([^"]*)"`)
	reTomlWireAPI  = regexp.MustCompile(`(?m)^(\s*wire_api\s*=\s*)"([^"]*)"`)
	reTomlProvider = regexp.MustCompile(`(?m)^(model_provider\s*=\s*)"([^"]*)"`)
)

// tomlSet 把 re 匹配到的第一处赋值改为 val，保留行首前缀；no-op 当无匹配。
func tomlSet(text string, re *regexp.Regexp, val string) (string, bool) {
	loc := re.FindStringSubmatchIndex(text)
	if loc == nil {
		return text, false
	}
	prefix := text[loc[2]:loc[3]]
	return text[:loc[0]] + prefix + fmt.Sprintf("%q", val) + text[loc[1]:], true
}

// writeCodexProviderTOML 维护账号池 config.toml 的中转站配置。已有文件做
// 行级原地替换（保留手工调优的其余键），没有才生成最小模板。
func writeCodexProviderTOML(credDir, baseURL, wireAPI string) error {
	path := filepath.Join(credDir, "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		text = fmt.Sprintf(`model_provider = "agentbox"
disable_response_storage = true

[model_providers.agentbox]
name = "agentbox"
base_url = %q
wire_api = %q
requires_openai_auth = true
`, baseURL, wireAPI)
		return os.WriteFile(path, []byte(text), 0o600)
	}

	if next, ok := tomlSet(text, reTomlBaseURL, baseURL); ok {
		text = next
		if next, ok := tomlSet(text, reTomlWireAPI, wireAPI); ok {
			text = next
		} else {
			// 有 base_url 没 wire_api 的旧文件：紧跟 base_url 行补一条
			loc := reTomlBaseURL.FindStringIndex(text)
			text = text[:loc[1]] + fmt.Sprintf("\nwire_api = %q", wireAPI) + text[loc[1]:]
		}
	} else {
		// 完全没有 provider 段：追加模板段并让顶层 model_provider 指过去
		if next, ok := tomlSet(text, reTomlProvider, "agentbox"); ok {
			text = next
		} else {
			text = "model_provider = \"agentbox\"\n" + text
		}
		text = strings.TrimRight(text, "\n") + fmt.Sprintf(`

[model_providers.agentbox]
name = "agentbox"
base_url = %q
wire_api = %q
requires_openai_auth = true
`, baseURL, wireAPI)
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

// readCodexProvider 从账号池 config.toml 读当前 base_url / wire_api，供
// 前端弹窗预填。文件缺失或没配返回空串。
func readCodexProvider(credDir string) (baseURL, wireAPI string) {
	if credDir == "" {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(credDir, "config.toml"))
	if err != nil {
		return "", ""
	}
	if m := reTomlBaseURL.FindSubmatch(raw); m != nil {
		baseURL = string(m[2])
	}
	if m := reTomlWireAPI.FindSubmatch(raw); m != nil {
		wireAPI = string(m[2])
	}
	return baseURL, wireAPI
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
			key = savedAPIKey(acct.CredentialsDir)
		}
		if baseURL == "" {
			baseURL, _ = readCodexProvider(acct.CredentialsDir)
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

	// 中转站的 base_url 有的带 /v1 有的不带，两种路径都试
	candidates := []string{baseURL + "/models"}
	if !strings.HasSuffix(baseURL, "/v1") {
		candidates = []string{baseURL + "/v1/models", baseURL + "/models"}
	}

	// 探测也走账号绑定的出口 IP，否则「这里能通」跟容器里能不能通是两回事。
	client, err := s.acctClient(acct, 20*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer client.CloseIdleConnections()
	var lastErr string
	for _, u := range candidates {
		start := time.Now()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if acct.Type == config.AgentClaude {
			// 官方 /v1/models 认 x-api-key + anthropic-version，中转站认 Bearer
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Sprintf("%s → HTTP %d: %s", u, resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
			continue
		}
		var lst struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &lst) != nil {
			lastErr = u + " → 响应不是模型列表 JSON"
			continue
		}
		models := make([]string, 0, len(lst.Data))
		for _, m := range lst.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}
		sort.Strings(models)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"endpoint":   u,
			"latency_ms": time.Since(start).Milliseconds(),
			"models":     models,
		})
		return
	}
	writeErr(w, http.StatusBadGateway, "连接失败: "+lastErr)
}

// savedAPIKey 读账号池 auth.json 里已保存的 key，探测时留空复用。
func savedAPIKey(credDir string) string {
	if credDir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(credDir, "auth.json"))
	if err != nil {
		return ""
	}
	var a struct {
		Key string `json:"OPENAI_API_KEY"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	return a.Key
}

// credStatus 汇总账号池凭证状态，供侧栏展示。
//   - claude: ok(有 refreshToken) / norefresh(令牌被清空需重登) / missing
//   - codex:  ok(auth.json 有 key) / missing
func credStatus(a config.Account) (status string, expiresAt int64) {
	if a.CredentialsDir == "" {
		return "missing", 0
	}
	switch a.Type {
	case config.AgentClaude:
		raw, err := os.ReadFile(filepath.Join(a.CredentialsDir, ".credentials.json"))
		if err != nil {
			return "missing", 0
		}
		var c struct {
			ClaudeAiOauth struct {
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(raw, &c) != nil || c.ClaudeAiOauth.RefreshToken == "" {
			return "norefresh", 0
		}
		return "ok", c.ClaudeAiOauth.ExpiresAt
	case config.AgentCodex:
		raw, err := os.ReadFile(filepath.Join(a.CredentialsDir, "auth.json"))
		if err != nil || !bytes.Contains(raw, []byte("OPENAI_API_KEY")) {
			return "missing", 0
		}
		return "ok", 0
	}
	return "missing", 0
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
