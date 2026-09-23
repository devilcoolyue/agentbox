package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
)

// 服务端续期前必须先把各会话 home 里的凭证收回池子：容器里的 CLI 刚续过的话，
// 池子里那份刷新令牌已经作废，拿它去换只会白挨一个 invalid_grant，而正确答案
// 就躺在会话 home 里。
func TestEnsureClaudeCredPrefersSessionCopy(t *testing.T) {
	s, sess := newTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(-time.Hour).UnixMilli())
	sess.Agent = config.AgentClaude
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}

	// 会话 home 里是 CLI 刚续出来的一份：更新的 mtime + 还没过期的令牌。
	homeCred := filepath.Join(s.homeDir(sess), ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(homeCred), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-from-cli","refreshToken":"r-cli","expiresAt":%d,"subscriptionType":"pro"}}`,
		time.Now().Add(4*time.Hour).UnixMilli())
	if err := os.WriteFile(homeCred, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(homeCred, now, now); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(acct.CredentialsDir, ".credentials.json"), old, old); err != nil {
		t.Fatal(err)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不该发起续期：会话里已经有更新的凭证")
	}))
	defer up.Close()
	claudeOAuthToken = up.URL
	t.Cleanup(func() { claudeOAuthToken = "https://platform.claude.com/v1/oauth/token" })

	cred, err := s.ensureClaudeCred(t.Context(), acct, false)
	if err != nil {
		t.Fatalf("ensureClaudeCred: %v", err)
	}
	if cred.AccessToken != "tok-from-cli" {
		t.Errorf("accessToken = %q, want tok-from-cli", cred.AccessToken)
	}
	// 顺带确认收敛是双向的：池子也被会话里那份盖上了。
	if c := poolCred(t, acct); c["accessToken"] != "tok-from-cli" {
		t.Errorf("池子没被会话里更新的那份收敛: %+v", c)
	}
}

// 刷新令牌已经没了就只能人来管，这时不该往上游打一枪再报错。
func TestEnsureClaudeCredWithoutRefreshToken(t *testing.T) {
	s, sess := newTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(-time.Hour).UnixMilli())
	cred := `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"","expiresAt":1}}`
	if err := os.WriteFile(filepath.Join(acct.CredentialsDir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("没有刷新令牌就不该打上游")
	}))
	defer up.Close()
	claudeOAuthToken = up.URL
	t.Cleanup(func() { claudeOAuthToken = "https://platform.claude.com/v1/oauth/token" })

	if _, err := s.ensureClaudeCred(t.Context(), acct, false); err == nil {
		t.Fatal("want error")
	} else if !strings.Contains(err.Error(), "重新登录") {
		t.Errorf("err = %v，提示应指向重新登录", err)
	}
}
