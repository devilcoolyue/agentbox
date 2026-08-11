package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	cred := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-abc","refreshToken":"r","expiresAt":%d,"subscriptionType":"pro"}}`, expiresAt)
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	acct := config.Account{ID: "claude-1", Type: config.AgentClaude, Label: "订阅", CredentialsDir: credDir}
	s.cfg.Accounts = []config.Account{acct}
	sess.AccountID = acct.ID
	return acct
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
	// 上游不该被打到：这几种情况都要在本地就挡下来。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer up.Close()
	claudeOAuthUsage = up.URL
	t.Cleanup(func() { claudeOAuthUsage = "https://api.anthropic.com/api/oauth/usage" })

	tests := []struct {
		name  string
		setup func(t *testing.T, s *Server, sess *store.Session)
	}{
		{"过期令牌", func(t *testing.T, s *Server, sess *store.Session) {
			seedClaudeAccount(t, s, sess, time.Now().Add(-time.Minute).UnixMilli())
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
