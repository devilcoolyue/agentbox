package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// 一份按真实 /api/oauth/usage 响应裁下来的样本：pro 账号只有 five_hour 和
// seven_day 有值，按模型细分的周窗口是 null，另外还有一批内部代号窗口。
const usageSample = `{
  "five_hour":  {"utilization": 22.0, "resets_at": "2026-08-10T14:19:59.117705+00:00"},
  "seven_day":  {"utilization": 4.0,  "resets_at": "2026-08-15T08:59:59.117725+00:00"},
  "seven_day_opus": null,
  "seven_day_sonnet": null,
  "tangelo": {"utilization": 99.0, "resets_at": null},
  "nimbus_quill": {"utilization": 0.0, "resets_at": null},
  "extra_usage": {
    "is_enabled": true, "monthly_limit": 3500, "used_credits": 1851.0,
    "utilization": 52.88571428571428, "currency": "USD", "decimal_places": 2
  }
}`

// seedClaudeAccount 造一个带 OAuth 凭证的 claude 账号，并把会话绑上去。
func seedClaudeAccount(t *testing.T, s *Server, sess *store.Session, expiresAt int64) config.Account {
	t.Helper()
	credDir := t.TempDir()
	cred := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-abc","refreshToken":"r","expiresAt":%d,"subscriptionType":"pro","rateLimitTier":"default_claude_max_20x"}}`, expiresAt)
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	acct := config.Account{ID: "claude-1", Type: config.AgentClaude, Label: "订阅", CredentialsDir: credDir}
	s.cfg.Accounts = []config.Account{acct}
	sess.AccountID = acct.ID
	return acct
}

// poolCred 读回账号池里那份凭证，用来断言续期确实落盘了。
func poolCred(t *testing.T, acct config.Account) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(acct.CredentialsDir, ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		ClaudeAiOauth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	return root.ClaudeAiOauth
}

// fakeOAuth 起一个同时假扮换令牌与额度两个端点的上游，并把两个全局地址指过去。
// usage 只认 wantToken，其余一律 401——这正是「令牌过期了」在上游侧的形状。
func fakeOAuth(t *testing.T, wantToken, tokenResp string, tokenCode int) (tokenHits, usageHits *int) {
	t.Helper()
	th, uh := 0, 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			th++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["grant_type"] != "refresh_token" || body["refresh_token"] == "" || body["client_id"] == "" {
				t.Errorf("refresh 请求体不对: %+v", body)
			}
			w.WriteHeader(tokenCode)
			fmt.Fprint(w, tokenResp)
		case "/usage":
			uh++
			if r.Header.Get("Authorization") != "Bearer "+wantToken {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":"expired"}`)
				return
			}
			fmt.Fprint(w, usageSample)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(up.Close)

	claudeOAuthUsage, claudeOAuthToken = up.URL+"/usage", up.URL+"/token"
	t.Cleanup(func() {
		claudeOAuthUsage = "https://api.anthropic.com/api/oauth/usage"
		claudeOAuthToken = "https://platform.claude.com/v1/oauth/token"
	})
	return &th, &uh
}

// 令牌过期时服务端自己拿刷新令牌续，用户不该看到「发一轮对话再来」。
func TestHandleAccountUsageAutoRefreshes(t *testing.T) {
	s, sess := newTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(-time.Hour).UnixMilli())
	// 刷新响应刻意不带 subscription_type / scopes：这些登录时才拿得到的字段
	// 必须从旧凭证里留下来，丢了容器里的 CLI 会把订阅号当 API 账号。
	tokenHits, _ := fakeOAuth(t, "tok-new",
		`{"access_token":"tok-new","refresh_token":"r-next","expires_in":28800}`, http.StatusOK)

	w := httptest.NewRecorder()
	s.handleAccountUsage(w, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/account/usage", nil), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if *tokenHits != 1 {
		t.Errorf("token endpoint hits = %d, want 1", *tokenHits)
	}

	var got acctUsageView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Plan != "pro" {
		t.Errorf("plan = %q, want pro（订阅档位应从旧凭证留存）", got.Plan)
	}

	c := poolCred(t, acct)
	if c["accessToken"] != "tok-new" || c["refreshToken"] != "r-next" {
		t.Errorf("池子里的令牌没换成新的: %+v", c)
	}
	if c["subscriptionType"] != "pro" || c["rateLimitTier"] != "default_claude_max_20x" {
		t.Errorf("续期把登录时才有的字段冲掉了: %+v", c)
	}
	exp, _ := c["expiresAt"].(float64)
	if int64(exp) <= time.Now().UnixMilli() {
		t.Errorf("expiresAt = %v，续期后应是将来的时间", c["expiresAt"])
	}
}

// 凭证里的 expiresAt 说还没过期、上游却回 401：强制续一次再重打，别直接
// 把用户打发去重新登录。
func TestHandleAccountUsageRetriesAfter401(t *testing.T) {
	s, sess := newTestServer(t)
	seedClaudeAccount(t, s, &sess, time.Now().Add(time.Hour).UnixMilli())
	tokenHits, usageHits := fakeOAuth(t, "tok-new",
		`{"access_token":"tok-new","refresh_token":"r-next","expires_in":28800}`, http.StatusOK)

	w := httptest.NewRecorder()
	s.handleAccountUsage(w, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/account/usage", nil), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if *tokenHits != 1 || *usageHits != 2 {
		t.Errorf("token/usage hits = %d/%d, want 1/2（401 后只续一次、只重打一次）", *tokenHits, *usageHits)
	}
}

// 刷新令牌也废了才是真的要人来管，这时的提示必须指向重新登录。
func TestHandleAccountUsageRefreshRejected(t *testing.T) {
	s, sess := newTestServer(t)
	seedClaudeAccount(t, s, &sess, time.Now().Add(-time.Hour).UnixMilli())
	fakeOAuth(t, "tok-new", `{"error":"invalid_grant"}`, http.StatusBadRequest)

	w := httptest.NewRecorder()
	s.handleAccountUsage(w, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/account/usage", nil), sess)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("status = %d, want 4xx: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "重新登录") {
		t.Errorf("续期失败的提示应指向重新登录: %s", w.Body.String())
	}
}

func TestHandleAccountUsageNormalizes(t *testing.T) {
	s, sess := newTestServer(t)
	seedClaudeAccount(t, s, &sess, time.Now().Add(time.Hour).UnixMilli())

	var gotAuth, gotBeta string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, usageSample)
	}))
	defer up.Close()
	claudeOAuthUsage = up.URL
	t.Cleanup(func() { claudeOAuthUsage = "https://api.anthropic.com/api/oauth/usage" })

	w := httptest.NewRecorder()
	s.handleAccountUsage(w, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/account/usage", nil), sess)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", gotAuth)
	}
	if gotBeta != claudeOAuthBeta {
		t.Errorf("anthropic-beta = %q, want %q", gotBeta, claudeOAuthBeta)
	}

	var got acctUsageView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Plan != "pro" || got.AccountLabel != "订阅" {
		t.Errorf("plan/label = %q/%q, want pro/订阅", got.Plan, got.AccountLabel)
	}
	// 只透出官方 /usage 展示的窗口：代号窗口（tangelo 之类）和 null 窗口都不要。
	if len(got.Windows) != 2 {
		t.Fatalf("windows = %+v, want just five_hour + seven_day", got.Windows)
	}
	if got.Windows[0].Key != "five_hour" || got.Windows[0].Percent != 22 {
		t.Errorf("five_hour = %+v", got.Windows[0])
	}
	if got.Windows[0].ResetsAt == "" {
		t.Error("five_hour resets_at dropped")
	}
	if got.Windows[1].Key != "seven_day" || got.Windows[1].Percent != 4 {
		t.Errorf("seven_day = %+v", got.Windows[1])
	}
	// decimal_places=2 → 3500 是 $35.00、1851 是 $18.51。
	if got.Extra == nil || got.Extra.Used != 18.51 || got.Extra.Limit != 35 {
		t.Errorf("extra = %+v, want used 18.51 / limit 35", got.Extra)
	}
}

func TestHandleAccountUsageRejects(t *testing.T) {
	// 上游不该被打到：这几种情况都要在本地就挡下来，包括不许悄悄发起续期。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer up.Close()
	claudeOAuthUsage, claudeOAuthToken = up.URL, up.URL
	t.Cleanup(func() {
		claudeOAuthUsage = "https://api.anthropic.com/api/oauth/usage"
		claudeOAuthToken = "https://platform.claude.com/v1/oauth/token"
	})

	tests := []struct {
		name  string
		setup func(t *testing.T, s *Server, sess *store.Session)
	}{
		{"过期且没有刷新令牌", func(t *testing.T, s *Server, sess *store.Session) {
			acct := seedClaudeAccount(t, s, sess, time.Now().Add(-time.Minute).UnixMilli())
			cred := `{"claudeAiOauth":{"accessToken":"tok-abc","refreshToken":"","expiresAt":1}}`
			if err := os.WriteFile(filepath.Join(acct.CredentialsDir, ".credentials.json"), []byte(cred), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"中转站账号", func(t *testing.T, s *Server, sess *store.Session) {
			acct := seedClaudeAccount(t, s, sess, time.Now().Add(time.Hour).UnixMilli())
			acct.Env = map[string]string{envAnthropicToken: "sk-relay"}
			s.cfg.Accounts = []config.Account{acct}
		}},
		{"codex 账号", func(t *testing.T, s *Server, sess *store.Session) {
			s.cfg.Accounts = []config.Account{{ID: "codex-1", Type: config.AgentCodex}}
			sess.AccountID = "codex-1"
		}},
		{"账号已删除", func(t *testing.T, s *Server, sess *store.Session) {
			sess.AccountID = "gone"
		}},
		{"从没登录过", func(t *testing.T, s *Server, sess *store.Session) {
			s.cfg.Accounts = []config.Account{{
				ID: "claude-1", Type: config.AgentClaude, CredentialsDir: t.TempDir(),
			}}
			sess.AccountID = "claude-1"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, sess := newTestServer(t)
			tc.setup(t, s, &sess)
			w := httptest.NewRecorder()
			s.handleAccountUsage(w, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/account/usage", nil), sess)
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("status = %d, want 4xx: %s", w.Code, w.Body.String())
			}
		})
	}
}
