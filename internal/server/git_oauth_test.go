package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
	"agentbox/internal/tunnel"
)

func gitOAuthFixture(t *testing.T, provider, base string, networks ...gitaccess.NetworkPolicy) (*Server, config.GitOAuthApp) {
	t.Helper()
	s, _ := newTestServer(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"auth_token": "fixture-admin-token", "data_dir": s.cfg.DataDir, "listen": "127.0.0.1:8180", "agent_image": "fixture", "max_upload_mb": 10})
	if err := os.WriteFile(cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	s.cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = s.store.CreateToken("fixture-login-token", "alice"); err != nil {
		t.Fatal(err)
	}
	var network gitaccess.NetworkPolicy
	if len(networks) > 0 {
		network = networks[0]
	}
	body, _ := json.Marshal(map[string]any{"label": "Company Git", "provider": provider, "base_url": base, "client_id": "fixture-client", "client_secret": "synthetic-app-secret", "redirect_url": "https://agentbox.example/api/git/oauth/callback", "enabled": true, "network": network})
	w := httptest.NewRecorder()
	s.handleGitOAuthApps(w, accessRequest("root", "PUT", "/git/oauth/apps", string(body)))
	if w.Code != 200 {
		t.Fatalf("app save: %d %s", w.Code, w.Body.String())
	}
	apps := s.cfg.GitOAuthAppList()
	if len(apps) != 1 {
		t.Fatal("missing app")
	}
	configRaw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(configRaw), "synthetic-app-secret") {
		t.Fatal("plaintext app secret persisted")
	}
	return s, apps[0]
}
func startGitOAuth(t *testing.T, s *Server, app config.GitOAuthApp, connection string) (string, *http.Cookie, url.Values) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"app_id": app.ID, "label": "Authorized connection", "read_only": true, "connection_id": connection})
	r := accessRequest("alice", "POST", "/git/oauth/start", string(body))
	r.Host = "agentbox.example"
	r.Header.Set("Authorization", "Bearer fixture-login-token")
	w := httptest.NewRecorder()
	s.handleGitOAuthStart(w, r)
	if w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("redirect_uri") != app.RedirectURL {
		t.Fatalf("unsafe authorization URL: %s", out.URL)
	}
	if app.Provider == "gitlab" && strings.Contains(q.Get("scope"), "write_repository") {
		t.Fatal("read-only requested GitLab write scope")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe cookie: %+v", cookies)
	}
	return q.Get("state"), cookies[0], q
}
func callbackGitOAuth(s *Server, state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "https://agentbox.example/api/git/oauth/callback?state="+url.QueryEscape(state)+"&code=fixture-code", nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.handleGitOAuthCallback(w, r)
	return w
}
func TestGitOAuthProvidersPKCERefreshReauthorizeRevoke(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			var challenge string
			var exchanges, refreshes, revokes atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/revoke") || r.Method == "DELETE" {
					revokes.Add(1)
					w.WriteHeader(200)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if r.Form.Get("client_id") != "fixture-client" || r.Form.Get("client_secret") != "synthetic-app-secret" {
					t.Error("bad client authentication")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Form.Get("grant_type") == "refresh_token" {
					refreshes.Add(1)
					if r.Form.Get("refresh_token") != "synthetic-refresh" {
						t.Error("refresh token lost")
					}
					_, _ = w.Write([]byte(`{"access_token":"synthetic-renewed","refresh_token":"synthetic-refresh-2","expires_in":3600}`))
					return
				}
				exchanges.Add(1)
				hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
					t.Error("PKCE verifier mismatch")
				}
				if r.Form.Get("code") != "fixture-code" || r.Form.Get("redirect_uri") != "https://agentbox.example/api/git/oauth/callback" {
					t.Error("exchange parameters changed")
				}
				_, _ = w.Write([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":1,"scope":"read_repository"}`))
			}))
			defer upstream.Close()
			s, app := gitOAuthFixture(t, provider, upstream.URL)
			s.gitHTTPClient = func(context.Context, store.GitConnection) (*http.Client, func(), error) {
				return upstream.Client(), func() {}, nil
			}
			state, cookie, q := startGitOAuth(t, s, app, "")
			challenge = q.Get("code_challenge")
			if w := callbackGitOAuth(s, state, nil); w.Code != 400 || exchanges.Load() != 0 {
				t.Fatal("callback accepted without cookie")
			}
			if w := callbackGitOAuth(s, state, cookie); w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-access") {
				t.Fatalf("callback: %d %s", w.Code, w.Body.String())
			}
			if w := callbackGitOAuth(s, state, cookie); w.Code != 400 || exchanges.Load() != 1 {
				t.Fatal("OAuth code replayed")
			}
			rows, err := s.store.GitConnections("alice")
			if err != nil || len(rows) != 1 || rows[0].AuthType != "oauth" {
				t.Fatalf("connections: %+v %v", rows, err)
			}
			c, err := s.store.GitConnection("alice", rows[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, token, err := s.gitCredential(t.Context(), c)
					if err != nil || token != "synthetic-renewed" {
						t.Errorf("refresh: %q %v", token, err)
					}
				}()
			}
			wg.Wait()
			if refreshes.Load() != 1 {
				t.Fatalf("duplicate rotating refresh: %d", refreshes.Load())
			}
			state, cookie, q = startGitOAuth(t, s, app, c.ID)
			challenge = q.Get("code_challenge")
			if w := callbackGitOAuth(s, state, cookie); w.Code != 200 {
				t.Fatalf("reauthorize: %d %s", w.Code, w.Body.String())
			}
			rows, err = s.store.GitConnections("alice")
			if err != nil || len(rows) != 1 || rows[0].ID != c.ID {
				t.Fatal("reauthorize lost connection identity")
			}
			w := httptest.NewRecorder()
			s.handleGitConnections(w, accessRequest("alice", "GET", "/git/connections", ""))
			if !strings.Contains(w.Body.String(), `"oauth_app_id"`) || strings.Contains(w.Body.String(), "synthetic-") {
				t.Fatalf("metadata: %s", w.Body.String())
			}
			current, _ := s.store.GitConnection("alice", c.ID)
			body, _ := json.Marshal(map[string]int64{"revision": current.Revision})
			req := accessRequest("alice", "POST", "/revoke", string(body))
			req.SetPathValue("connection", c.ID)
			w = httptest.NewRecorder()
			s.handleGitOAuthRevoke(w, req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"remote_revoked":true`) || revokes.Load() != 1 {
				t.Fatalf("revoke: %s", w.Body.String())
			}
			current, _ = s.store.GitConnection("alice", c.ID)
			if current.Enabled {
				t.Fatal("revoked connection still enabled")
			}
			if _, _, err = s.gitCredential(t.Context(), current); err == nil {
				t.Fatal("revoked credential delivered")
			}
		})
	}
}
func TestGitOAuthCallbackRejectsLogoutAndAppChange(t *testing.T) {
	s, app := gitOAuthFixture(t, "gitlab", "https://git.example.com")
	state, cookie, _ := startGitOAuth(t, s, app, "")
	app.Enabled = false
	if err := s.cfg.SaveGitOAuthApp(app, app.Revision); err != nil {
		t.Fatal(err)
	}
	if w := callbackGitOAuth(s, state, cookie); w.Code != 409 {
		t.Fatalf("changed app: %d", w.Code)
	}
	app, _ = s.cfg.GitOAuthApp(app.ID)
	app.Enabled = true
	if err := s.cfg.SaveGitOAuthApp(app, app.Revision); err != nil {
		t.Fatal(err)
	}
	app, _ = s.cfg.GitOAuthApp(app.ID)
	state, cookie, _ = startGitOAuth(t, s, app, "")
	if err := s.store.DeleteToken("fixture-login-token"); err != nil {
		t.Fatal(err)
	}
	if w := callbackGitOAuth(s, state, cookie); w.Code != 401 {
		t.Fatalf("logged-out callback: %d", w.Code)
	}
}

func TestGitOAuthCompanyRouteAndCA(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`))
	}))
	defer upstream.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))
	s, app := gitOAuthFixture(t, "gitlab", upstream.URL, gitaccess.NetworkPolicy{Route: "tunnel", CAPEM: ca})
	s.cfg.Tunnel.Enabled = true
	s.tunnels = newTunnelHub()
	state, cookie, _ := startGitOAuth(t, s, app, "")
	if w := callbackGitOAuth(s, state, cookie); w.Code != 502 || calls.Load() != 0 {
		t.Fatalf("offline OAuth contacted upstream: %d %d", w.Code, calls.Load())
	}
	allow, err := tunnel.ParseWhitelist([]string{strings.TrimPrefix(upstream.URL, "https://")})
	if err != nil {
		t.Fatal(err)
	}
	wireLink(t, s.tunnels, "alice", allow)
	waitReady(t, s.tunnels, "alice")
	state, cookie, _ = startGitOAuth(t, s, app, "")
	if w := callbackGitOAuth(s, state, cookie); w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("OAuth over user tunnel: %d %s", w.Code, w.Body.String())
	}
	rows, err := s.store.GitConnections("alice")
	if err != nil || len(rows) != 1 || rows[0].Network != app.Network {
		t.Fatalf("OAuth lost network policy: %+v %v", rows, err)
	}
}
