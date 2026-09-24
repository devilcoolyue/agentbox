package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
)

// Codex CLI's public OAuth client, also used by sub2api. The callback is on
// the user's machine; remote installations accept the complete URL by paste.
const (
	codexOAuthClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthAuthorize = "https://auth.openai.com/oauth/authorize"
	codexOAuthRedirect  = "http://localhost:1455/auth/callback"
)

var codexOAuthToken = "https://auth.openai.com/oauth/token"

func codexCallbackCode(raw, state string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.Host != "localhost:1455" || u.Path != "/auth/callback" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("请粘贴完整的 http://localhost:1455/auth/callback?... 回调地址")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["state"]) != 1 || q.Get("state") != state {
		return "", fmt.Errorf("state 不匹配，请使用本次授权的完整回调地址")
	}
	if q.Get("error") != "" {
		return "", fmt.Errorf("授权未完成，请重新打开授权链接并同意授权")
	}
	if len(q["code"]) != 1 || strings.TrimSpace(q.Get("code")) == "" {
		return "", fmt.Errorf("回调地址缺少授权码")
	}
	return q.Get("code"), nil
}

func (s *Server) handleCodexOAuthFinish(w http.ResponseWriter, r *http.Request, acct config.Account) {
	var body struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body) != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	oauthMu.Lock()
	pend, ok := oauthPend[acct.ID]
	if !ok || time.Since(pend.created) > oauthPendingTTL {
		oauthMu.Unlock()
		writeErr(w, http.StatusBadRequest, "授权已过期，请重新生成授权链接")
		return
	}
	if pend.busy {
		oauthMu.Unlock()
		writeErr(w, http.StatusConflict, "正在完成授权，请稍后重试")
		return
	}
	code, err := codexCallbackCode(body.Code, pend.state)
	if err != nil {
		oauthMu.Unlock()
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pend.busy = true
	oauthPend[acct.ID] = pend
	oauthMu.Unlock()
	defer func() {
		oauthMu.Lock()
		if p, exists := oauthPend[acct.ID]; exists && p.state == pend.state {
			p.busy = false
			oauthPend[acct.ID] = p
		}
		oauthMu.Unlock()
	}()

	client, err := s.acctClient(acct, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer client.CloseIdleConnections()
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {codexOAuthClientID},
		"code": {code}, "redirect_uri": {codexOAuthRedirect}, "code_verifier": {pend.verifier},
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, codexOAuthToken, strings.NewReader(form.Encode()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法构造令牌请求")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "请求令牌接口失败，请检查账号出口代理或网络")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Never return raw upstream bodies: they may contain tokens or codes.
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("换取令牌失败 (HTTP %d)，请重新授权", resp.StatusCode))
		return
	}
	var tok credentials.CodexOAuthTokens
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok) != nil || tok.AccessToken == "" || tok.RefreshToken == "" || tok.IDToken == "" {
		writeErr(w, http.StatusBadGateway, "令牌响应不完整，请重新授权")
		return
	}
	// Code was consumed upstream. Never exchange it again, even if disk save fails.
	oauthMu.Lock()
	delete(oauthPend, acct.ID)
	oauthMu.Unlock()
	if err := s.credentialService().SaveCodexOAuth(r.Context(), acct.ID, tok); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存凭证失败，请重新授权："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
