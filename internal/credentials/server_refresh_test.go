package credentials

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
)

func credJSON(access, refresh string, expiresAt int64, extra string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"subscriptionType":"max","scopes":["user:inference"]}%s}`, access, refresh, expiresAt, extra)
}

func markPool(t *testing.T, acct config.Account) {
	t.Helper()
	put(t, filepath.Join(acct.CredentialsDir, agent.ServerRefreshMarker), "test\n")
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func TestServerRefreshedHomeNeverFeedsPool(t *testing.T) {
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	svc, data := testService(t, "https://not-called.invalid", acct)
	later := time.Now().Add(4 * time.Hour).UnixMilli()
	pool := filepath.Join(acct.CredentialsDir, ".credentials.json")
	poolRaw := credJSON("pool-access", "pool-refresh", later, "")
	put(t, pool, poolRaw)
	markPool(t, acct)
	// A user forges a login with a refresh token and a far-future mtime, and
	// has signed in to an MCP server of their own from this workspace.
	home := homeFile(data, "alice", "s1")
	put(t, home, credJSON("forged-access", "forged-refresh", later, `,"mcpOAuth":{"github":{"accessToken":"alice-mcp"}}`))
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(home, future, future); err != nil {
		t.Fatal(err)
	}

	if err := svc.Sync(t.Context(), acct, svc.sessions.All()[0]); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(pool); string(got) != poolRaw {
		t.Fatalf("pool adopted session content: %s", got)
	}
	got := readJSON(t, home)
	login := got["claudeAiOauth"].(map[string]any)
	if login["accessToken"] != "pool-access" || login["subscriptionType"] != "max" {
		t.Fatalf("forged login not replaced: %v", login)
	}
	if _, ok := login["refreshToken"]; ok {
		t.Fatalf("home holds a refresh token: %v", login)
	}
	if mcp, _ := got["mcpOAuth"].(map[string]any); mcp["github"] == nil {
		t.Fatalf("session's own MCP sign-in was dropped: %v", got)
	}
	info, err := os.Stat(home)
	if err != nil || !info.ModTime().Before(future) {
		t.Fatalf("rewrite must move mtime so a running CLI rereads it: %v %v", info.ModTime(), err)
	}

	// Converged: a second pass leaves the file alone.
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(home, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := svc.Sync(t.Context(), acct, svc.sessions.All()[0]); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(home); !info.ModTime().Equal(stamp) {
		t.Fatal("converged home rewritten")
	}
}

func TestPoolExtrasNeverReachHomes(t *testing.T) {
	// Older releases adopted whole session files, so a pool may already hold
	// one user's MCP tokens. They must not spread any further.
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	svc, data := testService(t, "https://not-called.invalid", acct)
	later := time.Now().Add(4 * time.Hour).UnixMilli()
	put(t, filepath.Join(acct.CredentialsDir, ".credentials.json"), credJSON("pool-access", "pool-refresh", later, `,"mcpOAuth":{"github":{"accessToken":"someone-else"}}`))
	markPool(t, acct)
	bob := homeFile(data, "bob", "s2")
	put(t, bob, credJSON("old-access", "old-refresh", 1, ""))
	svc.broadcast(acct)
	raw, _ := os.ReadFile(bob)
	if strings.Contains(string(raw), "someone-else") || strings.Contains(string(raw), "refresh") {
		t.Fatalf("pool extras or refresh token published: %s", raw)
	}
	if !strings.Contains(string(raw), "pool-access") {
		t.Fatalf("login not published: %s", raw)
	}
}

func TestLegacyAccountMigratesOnFirstRefresh(t *testing.T) {
	var seen atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen.Store(req["refresh_token"])
		fmt.Fprint(w, `{"access_token":"server-access","refresh_token":"server-refresh","expires_in":28800}`)
	}))
	defer upstream.Close()
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	svc, data := testService(t, upstream.URL, acct)
	later := time.Now().Add(4 * time.Hour).UnixMilli()
	pool := filepath.Join(acct.CredentialsDir, ".credentials.json")
	put(t, pool, credJSON("stale-access", "stale-refresh", later, ""))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(pool, old, old); err != nil {
		t.Fatal(err)
	}
	// A CLI rotated the chain just before the upgrade; the pool copy is dead.
	alice := homeFile(data, "alice", "s1")
	put(t, alice, credJSON("cli-access", "cli-refresh", later, ""))

	svc.SyncAll(t.Context())

	if seen.Load() != "cli-refresh" {
		t.Fatalf("migration refreshed with %v, want the session's newer chain", seen.Load())
	}
	if _, err := os.Stat(filepath.Join(acct.CredentialsDir, agent.ServerRefreshMarker)); err != nil {
		t.Fatalf("account not marked: %v", err)
	}
	for _, home := range []string{alice} {
		raw, _ := os.ReadFile(home)
		if !strings.Contains(string(raw), "server-access") || strings.Contains(string(raw), "refresh") {
			t.Fatalf("home after migration: %s", raw)
		}
	}
	if !strings.Contains(readFile(t, pool), "server-refresh") {
		t.Fatal("pool lost the server's chain")
	}
}

func TestRefreshAheadTimingAndBackoff(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"access_token":"next-access","refresh_token":"next-refresh","expires_in":28800}`)
	}))
	defer upstream.Close()
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	svc, data := testService(t, upstream.URL, acct)
	pool := filepath.Join(acct.CredentialsDir, ".credentials.json")
	markPool(t, acct)
	put(t, homeFile(data, "alice", "s1"), credJSON("x", "", 1, ""))

	put(t, pool, credJSON("a", "r", time.Now().Add(2*time.Hour).UnixMilli(), ""))
	svc.SyncAll(t.Context())
	if calls.Load() != 0 {
		t.Fatal("refreshed a token with hours left")
	}

	put(t, pool, credJSON("a", "r", time.Now().Add(20*time.Minute).UnixMilli(), ""))
	svc.SyncAll(t.Context())
	if calls.Load() != 1 {
		t.Fatalf("did not refresh ahead of expiry: %d", calls.Load())
	}
	if !strings.Contains(readFile(t, homeFile(data, "alice", "s1")), "next-access") {
		t.Fatal("refreshed token not published")
	}

	// Min interval: even if the new token were already near expiry, no spin.
	put(t, pool, credJSON("b", "r2", time.Now().Add(time.Minute).UnixMilli(), ""))
	svc.SyncAll(t.Context())
	if calls.Load() != 1 {
		t.Fatal("refreshed again inside the minimum interval")
	}

	// Failures back off instead of hammering the token endpoint every pass.
	svc.refreshes.Delete(acct.ID)
	fail.Store(true)
	svc.SyncAll(t.Context())
	svc.SyncAll(t.Context())
	if calls.Load() != 2 {
		t.Fatalf("failed refresh retried without backoff: %d", calls.Load())
	}
	v, _ := svc.refreshes.Load(acct.ID)
	if s := v.(refreshSchedule); s.backoff != serverRetryMin || time.Until(s.next) <= 0 {
		t.Fatalf("schedule after failure: %+v", s)
	}
}

func TestRefreshAheadSkipsAccountsWithoutChain(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	codex := config.Account{ID: "codex", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	bare := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	svc, _ := testService(t, upstream.URL, codex, bare)
	put(t, filepath.Join(bare.CredentialsDir, ".credentials.json"), credJSON("a", "", 1, ""))
	svc.SyncAll(t.Context())
	if calls.Load() != 0 {
		t.Fatal("refresh attempted without a refresh token or for Codex")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
