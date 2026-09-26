package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
)

type gitOAuthState struct {
	mu           sync.Mutex
	pending      map[string]gitOAuthPending
	refreshLocks sync.Map
}
type gitOAuthPending struct {
	actor, token, app, label, verifier, cookie string
	connection                                 string
	connectionRevision                         int64
	revision                                   int64
	readOnly                                   bool
	expires                                    time.Time
}
type gitOAuthCredential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	AppID        string `json:"app_id"`
	Scope        string `json:"scope,omitempty"`
}

func oauthRandom() string {
	v := make([]byte, 32)
	if _, err := rand.Read(v); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(v)
}
func (s *Server) handleGitOAuthApps(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		out := []map[string]any{}
		for _, a := range s.cfg.GitOAuthAppList() {
			if !a.Enabled && reqUser(r).Role != store.RoleAdmin {
				continue
			}
			row := map[string]any{"id": a.ID, "label": a.Label, "provider": a.Provider, "base_url": a.BaseURL, "enabled": a.Enabled, "network": a.Network}
			if reqUser(r).Role == store.RoleAdmin {
				row["client_id"] = a.ClientID
				row["redirect_url"] = a.RedirectURL
				row["revision"] = a.Revision
			}
			out = append(out, row)
		}
		writeJSON(w, 200, out)
		return
	}
	var input struct {
		ID           string                  `json:"id"`
		Label        string                  `json:"label"`
		Provider     string                  `json:"provider"`
		BaseURL      string                  `json:"base_url"`
		ClientID     string                  `json:"client_id"`
		ClientSecret string                  `json:"client_secret"`
		RedirectURL  string                  `json:"redirect_url"`
		Enabled      bool                    `json:"enabled"`
		Revision     int64                   `json:"revision"`
		Network      gitaccess.NetworkPolicy `json:"network"`
	}
	if !decodeGitJSON(w, r, &input) {
		return
	}
	if !gitText(input.Label, 128, false) || !gitText(input.ClientID, 256, false) || !gitText(input.ClientSecret, 8192, true) {
		writeErr(w, 400, "OAuth 应用字段无效")
		return
	}
	a := config.GitOAuthApp{ID: input.ID, Label: input.Label, Provider: input.Provider, BaseURL: strings.TrimRight(input.BaseURL, "/"), ClientID: input.ClientID, RedirectURL: input.RedirectURL, Enabled: input.Enabled, Network: input.Network}
	if a.ID == "" {
		a.ID = "git-" + store.NewID()
	}
	if old, exists := s.cfg.GitOAuthApp(a.ID); exists {
		a.Secret = old.Secret
		if input.Revision != old.Revision {
			writeErr(w, 409, config.ErrGitOAuthConflict.Error())
			return
		}
	}
	if input.ClientSecret != "" {
		n, err := s.store.GitConnectionCount()
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
		a.Secret, err = s.gitVault().Seal([]byte(input.ClientSecret), a.AssociatedData(), n == 0 && len(s.cfg.GitOAuthAppList()) == 0)
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
	}
	if err := s.cfg.SaveGitOAuthApp(a, input.Revision); err != nil {
		code := 400
		if errors.Is(err, config.ErrGitOAuthConflict) {
			code = 409
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleGitOAuthStart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AppID        string `json:"app_id"`
		Label        string `json:"label"`
		ReadOnly     *bool  `json:"read_only"`
		APIAccess    bool   `json:"api_access"`
		ConnectionID string `json:"connection_id"`
	}
	if !decodeGitJSON(w, r, &input) {
		return
	}
	app, ok := s.cfg.GitOAuthApp(input.AppID)
	if !ok || !app.Enabled {
		writeErr(w, 404, "OAuth 应用不存在或已停用")
		return
	}
	if input.Label == "" {
		input.Label = app.Label
	}
	if !gitText(input.Label, 128, false) {
		writeErr(w, 400, "连接名称无效")
		return
	}
	readOnly := true
	if input.ReadOnly != nil {
		readOnly = *input.ReadOnly
	}
	var connectionRevision int64
	if input.ConnectionID != "" {
		connection, err := s.store.GitConnection(reqUser(r).Name, input.ConnectionID)
		if err != nil || connection.AuthType != "oauth" {
			writeErr(w, 404, "OAuth 连接不存在")
			return
		}
		raw, err := s.gitVault().Open(connection.Secret, connection.AssociatedData())
		var credential gitOAuthCredential
		if err != nil || json.Unmarshal(raw, &credential) != nil || credential.AppID != app.ID {
			writeErr(w, 409, "请使用原 OAuth 应用重新授权此连接")
			return
		}
		connectionRevision = connection.Revision
	}
	redirect, _ := url.Parse(app.RedirectURL)
	if r.Host != redirect.Host {
		writeErr(w, 409, "当前访问域名与 OAuth 回调地址不一致，请通过配置的域名访问")
		return
	}
	if _, err := s.gitVault().Open(app.Secret, app.AssociatedData()); err != nil {
		writeGitStoreErr(w, err)
		return
	}
	state, verifier, cookie := oauthRandom(), oauthRandom(), oauthRandom()
	sum := sha256.Sum256([]byte(verifier))
	pending := gitOAuthPending{actor: reqUser(r).Name, token: bearerToken(r), app: app.ID, label: input.Label, verifier: verifier, cookie: cookie, revision: app.Revision, readOnly: readOnly, connection: input.ConnectionID, connectionRevision: connectionRevision, expires: time.Now().Add(10 * time.Minute)}
	s.gitOAuth.mu.Lock()
	if s.gitOAuth.pending == nil {
		s.gitOAuth.pending = map[string]gitOAuthPending{}
	}
	n := 0
	for key, p := range s.gitOAuth.pending {
		if time.Now().After(p.expires) {
			delete(s.gitOAuth.pending, key)
		} else if p.actor == pending.actor {
			n++
		}
	}
	if n >= 4 || len(s.gitOAuth.pending) >= 1000 {
		s.gitOAuth.mu.Unlock()
		writeErr(w, 429, "授权请求过多，请完成现有授权或稍后重试")
		return
	}
	s.gitOAuth.pending[state] = pending
	s.gitOAuth.mu.Unlock()
	scope := "repo read:user"
	endpoint := "/login/oauth/authorize"
	if app.Provider == "gitlab" {
		endpoint = "/oauth/authorize"
		scope = "read_user read_repository"
		if !readOnly {
			scope += " write_repository"
		}
		if input.APIAccess {
			if readOnly {
				scope += " read_api"
			} else {
				scope += " api"
			}
		}
	}
	q := url.Values{"client_id": {app.ClientID}, "redirect_uri": {app.RedirectURL}, "response_type": {"code"}, "scope": {scope}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	http.SetCookie(w, &http.Cookie{Name: "abox_git_oauth_" + state[:12], Value: cookie, Path: "/api/git/oauth/callback", HttpOnly: true, Secure: redirect.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: 600})
	writeJSON(w, 200, map[string]string{"url": app.BaseURL + endpoint + "?" + q.Encode(), "scope": scope})
}
func oauthEndpoint(a config.GitOAuthApp) string {
	if a.Provider == "gitlab" {
		return a.BaseURL + "/oauth/token"
	}
	return a.BaseURL + "/login/oauth/access_token"
}
func (s *Server) gitOAuthExchange(ctx context.Context, a config.GitOAuthApp, owner string, values url.Values) (gitOAuthCredential, error) {
	var credential gitOAuthCredential
	secret, err := s.gitVault().Open(a.Secret, a.AssociatedData())
	if err != nil {
		return credential, err
	}
	values.Set("client_id", a.ClientID)
	values.Set("client_secret", string(secret))
	client, closeClient, err := s.gitClient(ctx, store.GitConnection{Owner: owner, Provider: a.Provider, BaseURL: a.BaseURL, Network: a.Network})
	if err != nil {
		return credential, err
	}
	defer closeClient()
	safe := *client
	safe.Jar = nil
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, "POST", oauthEndpoint(a), strings.NewReader(values.Encode()))
	if err != nil {
		return credential, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := safe.Do(req)
	if err != nil {
		return credential, errors.New("OAuth 服务连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return credential, errors.New("OAuth 服务拒绝令牌交换，请重新授权或检查应用配置")
	}
	var body struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
		Scope   string `json:"scope"`
		Error   string `json:"error"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || json.Unmarshal(raw, &body) != nil || body.Error != "" || !gitText(body.Access, 16384, false) || !gitText(body.Refresh, 16384, true) || body.Expires < 0 || body.Expires > 365*24*3600 {
		return credential, errors.New("OAuth 返回无效令牌，请重新授权")
	}
	credential = gitOAuthCredential{AccessToken: body.Access, RefreshToken: body.Refresh, Scope: body.Scope, AppID: a.ID}
	if body.Expires > 0 {
		credential.ExpiresAt = time.Now().Add(time.Duration(body.Expires) * time.Second).Unix()
	}
	return credential, nil
}
func (s *Server) handleGitOAuthCallback(w http.ResponseWriter, r *http.Request) {
	// Static text only: never reflect provider error strings, codes, state or tokens.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	finish := func(code int, text string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		_, _ = fmt.Fprintln(w, text)
	}
	state := r.URL.Query().Get("state")
	if len(state) != 43 || len(r.URL.Query()["state"]) != 1 {
		finish(400, "授权状态无效，请返回 Agentbox 重新发起授权。")
		return
	}
	s.gitOAuth.mu.Lock()
	pending, ok := s.gitOAuth.pending[state]
	cookie, err := r.Cookie("abox_git_oauth_" + state[:12])
	if !ok || time.Now().After(pending.expires) || err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(pending.cookie)) != 1 {
		s.gitOAuth.mu.Unlock()
		finish(400, "授权已过期或浏览器不匹配，请重新发起授权。")
		return
	}
	delete(s.gitOAuth.pending, state)
	s.gitOAuth.mu.Unlock()
	a, ok := s.cfg.GitOAuthApp(pending.app)
	if !ok || !a.Enabled || a.Revision != pending.revision {
		finish(409, "OAuth 应用已变化，请重新发起授权。")
		return
	}
	redirect, _ := url.Parse(a.RedirectURL)
	http.SetCookie(w, &http.Cookie{Name: "abox_git_oauth_" + state[:12], Value: "", Path: "/api/git/oauth/callback", HttpOnly: true, Secure: redirect.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: -1})
	user, loggedIn := s.store.TokenUser(pending.token)
	if r.Host != redirect.Host || !loggedIn || user.Name != pending.actor {
		finish(401, "原登录会话已失效，请重新登录后授权。")
		return
	}
	if r.URL.Query().Get("error") != "" {
		finish(400, "授权未完成，请返回 Agentbox 重试。")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 4096 || len(r.URL.Query()["code"]) != 1 {
		finish(400, "缺少有效授权码。")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	credential, err := s.gitOAuthExchange(ctx, a, pending.actor, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.RedirectURL}, "code_verifier": {pending.verifier}})
	if err != nil {
		finish(502, "授权交换失败，请返回 Agentbox 检查应用设置后重试。")
		return
	}
	// Recheck state after the external request; admin disable/logout may race it.
	current, ok := s.cfg.GitOAuthApp(a.ID)
	if !ok || !current.Enabled || current.Revision != a.Revision {
		finish(409, "应用已停用或变更，授权未保存。")
		return
	}
	if u, valid := s.store.TokenUser(pending.token); !valid || u.Name != pending.actor {
		finish(401, "登录已失效，授权未保存。")
		return
	}
	c := store.GitConnection{ID: store.NewID() + store.NewID(), Owner: pending.actor, Label: pending.label, Provider: a.Provider, BaseURL: a.BaseURL, AuthType: "oauth", Network: a.Network, Username: "x-access-token", ReadOnly: pending.readOnly, Enabled: true}
	if a.Provider == "gitlab" {
		c.Username = "oauth2"
	}
	expected := int64(0)
	if pending.connection != "" {
		old, err := s.store.GitConnection(pending.actor, pending.connection)
		if err != nil || old.Revision != pending.connectionRevision || old.AuthType != "oauth" || old.BaseURL != a.BaseURL || old.Provider != a.Provider || old.Network != a.Network {
			finish(409, "连接已变化，授权未覆盖，请重试。")
			return
		}
		c.ID = old.ID
		c.CreatedAt = old.CreatedAt
		expected = old.Revision
	}
	encoded, _ := json.Marshal(credential)
	c.Secret, err = s.gitVault().Seal(encoded, c.AssociatedData(), false)
	if err == nil {
		_, err = s.store.SaveGitConnectionWithLogin(c, expected, pending.token)
	}
	if err != nil {
		finish(500, "保存授权失败，请返回 Agentbox 重试。")
		return
	}
	finish(200, "Git 授权已保存。请关闭此页面，返回 Agentbox 刷新 Git 连接列表并绑定仓库。")
}

// Refreshes are serialized per connection; PAT values remain raw bytes for
// compatibility with the first HTTPS implementation.
func (s *Server) gitCredential(ctx context.Context, c store.GitConnection) (store.GitConnection, string, error) {
	lock, _ := s.gitOAuth.refreshLocks.LoadOrStore(c.ID, make(chan struct{}, 1))
	ch := lock.(chan struct{})
	select {
	case ch <- struct{}{}:
		defer func() { <-ch }()
	case <-ctx.Done():
		return c, "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return c, "", err
	}
	current, err := s.store.GitConnectionFor(gitActor(c), c.ID)
	if err != nil {
		return c, "", err
	}
	c = current
	if !c.Enabled {
		return c, "", errors.New("Git 连接已停用")
	}
	raw, err := s.gitVault().Open(c.Secret, c.AssociatedData())
	if err != nil {
		return c, "", err
	}
	if c.AuthType == "pat" {
		return c, string(raw), nil
	}
	if c.AuthType != "oauth" {
		return c, "", errors.New("未知 Git 认证类型")
	}
	var credential gitOAuthCredential
	if json.Unmarshal(raw, &credential) != nil || credential.AccessToken == "" {
		return c, "", errors.New("OAuth 凭证格式无效，请重新授权")
	}
	app, ok := s.cfg.GitOAuthApp(credential.AppID)
	if !ok || !app.Enabled || app.Provider != c.Provider || app.BaseURL != c.BaseURL || app.Network != c.Network {
		return c, "", errors.New("对应 OAuth 应用不可用，请联系管理员")
	}
	if credential.ExpiresAt == 0 || credential.ExpiresAt > time.Now().Add(time.Minute).Unix() {
		return c, credential.AccessToken, nil
	}
	if credential.RefreshToken == "" {
		return c, "", errors.New("OAuth 已到期，请重新授权")
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	next, err := s.gitOAuthExchange(refreshCtx, app, c.Owner, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.RefreshToken}, "redirect_uri": {app.RedirectURL}})
	if err != nil {
		return c, "", errors.New("OAuth 续期失败，请重新授权")
	}
	if next.RefreshToken == "" {
		next.RefreshToken = credential.RefreshToken
	}
	if next.Scope == "" {
		next.Scope = credential.Scope
	}
	appNow, ok := s.cfg.GitOAuthApp(app.ID)
	if !ok || !appNow.Enabled || appNow.Revision != app.Revision {
		return c, "", errors.New("OAuth 应用已变化，请重新授权")
	}
	encoded, _ := json.Marshal(next)
	c.Secret, err = s.gitVault().Seal(encoded, c.AssociatedData(), false)
	if err != nil {
		return c, "", err
	}
	saved, err := s.store.SaveGitConnection(c, c.Revision)
	if err != nil {
		return c, "", err
	}
	saved.Secret = c.Secret
	return saved, next.AccessToken, nil
}

func (s *Server) handleGitOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision int64 `json:"revision"`
	}
	if !decodeGitJSON(w, r, &input) {
		return
	}
	c, err := s.store.GitConnection(reqUser(r).Name, r.PathValue("connection"))
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	if c.AuthType != "oauth" {
		writeErr(w, 400, "手工 Token 请到上游平台撤销；本地可停用此连接")
		return
	}
	if input.Revision != c.Revision {
		writeGitStoreErr(w, store.ErrGitConflict)
		return
	}
	// Local access is disabled even when upstream is offline; don't confuse that
	// with a successful upstream revocation. Preserve ciphertext for retry.
	c.Enabled = false
	if _, err = s.store.SaveGitConnection(c, input.Revision); err != nil {
		writeGitStoreErr(w, err)
		return
	}
	remoteRevoked := false
	defer func() {
		warning := ""
		if !remoteRevoked {
			warning = "本地连接已停用，但上游撤销未确认；请稍后重试或到服务平台撤销应用授权"
		}
		writeJSON(w, 200, map[string]any{"local_disabled": true, "remote_revoked": remoteRevoked, "warning": warning})
	}()
	raw, err := s.gitVault().Open(c.Secret, c.AssociatedData())
	if err != nil {
		return
	}
	var credential gitOAuthCredential
	if json.Unmarshal(raw, &credential) != nil {
		return
	}
	app, ok := s.cfg.GitOAuthApp(credential.AppID)
	if !ok {
		return
	}
	appSecret, err := s.gitVault().Open(app.Secret, app.AssociatedData())
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	client, closeClient, err := s.gitClient(ctx, c)
	if err != nil {
		return
	}
	defer closeClient()
	safe := *client
	safe.Jar = nil
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var req *http.Request
	if app.Provider == "gitlab" {
		token := credential.RefreshToken
		if token == "" {
			token = credential.AccessToken
		}
		form := url.Values{"client_id": {app.ClientID}, "client_secret": {string(appSecret)}, "token": {token}}
		req, err = http.NewRequestWithContext(ctx, "POST", app.BaseURL+"/oauth/revoke", strings.NewReader(form.Encode()))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		base := app.BaseURL + "/api/v3"
		if app.BaseURL == "https://github.com" {
			base = "https://api.github.com"
		}
		body, _ := json.Marshal(map[string]string{"access_token": credential.AccessToken})
		req, err = http.NewRequestWithContext(ctx, "DELETE", base+"/applications/"+url.PathEscape(app.ClientID)+"/token", strings.NewReader(string(body)))
		if err != nil {
			return
		}
		req.SetBasicAuth(app.ClientID, string(appSecret))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	op, err := s.beginGitOperation(ctx, c.Owner, "", "", c.ID, "oauth.revoke", app.ID)
	if err != nil {
		return
	}
	result := "failed_unknown"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	resp, err := safe.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	remoteRevoked = resp.StatusCode == 200 || resp.StatusCode == 204
	if remoteRevoked {
		result = "success"
	}
}
