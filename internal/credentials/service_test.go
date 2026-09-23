package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

type testSessions []store.Session

func (s testSessions) All() []store.Session { return s }
func (s testSessions) GetUser(name string) (store.User, bool) {
	return store.User{Name: name, Role: store.RoleUser}, true
}

func testService(t *testing.T, endpoint string, accounts ...config.Account) (*Service, string) {
	t.Helper()
	data := t.TempDir()
	cfg := &config.Config{DataDir: data, Accounts: accounts}
	svc := New(cfg, testSessions{
		{ID: "s1", User: "alice", Agent: config.AgentClaude, AccountID: "acct"},
		{ID: "s2", User: "bob", Agent: config.AgentClaude, AccountID: "acct"},
	}, func(_ config.Account, d time.Duration) (*http.Client, error) { return &http.Client{Timeout: d}, nil }, "test-public-client", func() string { return endpoint })
	return svc, data
}
func put(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}
func homeFile(data, user, id string) string {
	return filepath.Join(data, "users", user, "sessions", id, "home", ".claude", ".credentials.json")
}
func expiredJSON() string {
	return `{"claudeAiOauth":{"accessToken":"old-test-access","refreshToken":"old-test-refresh","expiresAt":1,"subscriptionType":"max","rateLimitTier":"test-tier","unknown":"keep"}}`
}

func TestConcurrentEnsureRefreshesOnceAndImmediatelyBroadcasts(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req["refresh_token"] != "old-test-refresh" {
			t.Errorf("unexpected refresh chain: %s", req["refresh_token"])
		}
		fmt.Fprint(w, `{"access_token":"fresh-test-access","refresh_token":"fresh-test-refresh","expires_in":3600}`)
	}))
	defer upstream.Close()
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir(), Access: &config.AccountAccess{Mode: "users", Users: []string{"alice"}}}
	svc, data := testService(t, upstream.URL, acct)
	pool := filepath.Join(acct.CredentialsDir, ".credentials.json")
	put(t, pool, expiredJSON())
	alice := homeFile(data, "alice", "s1")
	bob := homeFile(data, "bob", "s2")
	put(t, alice, expiredJSON())
	put(t, bob, expiredJSON())
	// Same timestamps reproduce the previous 2s suppression of immediate delivery.
	stamp := time.Now()
	for _, p := range []string{pool, alice, bob} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cred, err := svc.EnsureClaude(t.Context(), acct, false)
			if err != nil || cred.AccessToken != "fresh-test-access" {
				t.Errorf("refresh: %v %+v", err, cred)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("rotated %d times", calls.Load())
	}
	raw, _ := os.ReadFile(alice)
	if !strings.Contains(string(raw), "fresh-test-refresh") {
		t.Fatal("authorized home retained old chain")
	}
	raw, _ = os.ReadFile(bob)
	if strings.Contains(string(raw), "fresh-test") {
		t.Fatal("revoked user received new credentials")
	}
	raw, _ = os.ReadFile(pool)
	if !strings.Contains(string(raw), "test-tier") || !strings.Contains(string(raw), "keep") {
		t.Fatal("refresh lost metadata")
	}
}

func TestSyncWaitIsCancelableAndCannotRaceRefresh(t *testing.T) {
	entered := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-releaseUpstream
		fmt.Fprint(w, `{"access_token":"new-test","refresh_token":"new-refresh-test","expires_in":3600}`)
	}))
	defer upstream.Close()
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	svc, data := testService(t, upstream.URL, acct)
	put(t, filepath.Join(acct.CredentialsDir, ".credentials.json"), expiredJSON())
	put(t, homeFile(data, "alice", "s1"), expiredJSON())
	done := make(chan error, 1)
	go func() { _, err := svc.EnsureClaude(t.Context(), acct, false); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := svc.Lock(ctx, acct.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("account lock not shared/cancelable: %v", err)
	}
	syncDone := make(chan struct{})
	go func() { svc.Sync(ctx, acct, svc.sessions.All()[0]); close(syncDone) }()
	select {
	case <-syncDone:
	case <-time.After(time.Second):
		t.Fatal("canceled sync left waiting")
	}
	close(releaseUpstream)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRefreshDoesNotBypassFailedProxy(t *testing.T) {
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	svc, _ := testService(t, "https://must-not-be-called.invalid", acct)
	put(t, filepath.Join(acct.CredentialsDir, ".credentials.json"), expiredJSON())
	denied := errors.New("bound proxy unavailable")
	svc.client = func(config.Account, time.Duration) (*http.Client, error) { return nil, denied }
	if _, err := svc.EnsureClaude(t.Context(), acct, false); !errors.Is(err, denied) {
		t.Fatalf("proxy failure lost: %v", err)
	}
	cred, err := ReadClaude(acct)
	if err != nil || cred.AccessToken != "old-test-access" {
		t.Fatal("failed refresh mutated credentials")
	}
}

func TestCredentialReadsRejectLinksAndOversize(t *testing.T) {
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	outside := filepath.Join(t.TempDir(), "outside")
	put(t, outside, expiredJSON())
	name := filepath.Join(acct.CredentialsDir, ".credentials.json")
	if err := os.Symlink(outside, name); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClaude(acct); err == nil {
		t.Fatal("read followed credential symlink")
	}
	if st, _ := Status(acct); st != "missing" {
		t.Fatal("status followed credential symlink")
	}
	os.Remove(name)
	put(t, name, strings.Repeat(" ", credentialMaxBytes+1))
	if _, err := ReadClaude(acct); err == nil {
		t.Fatal("oversized credential accepted")
	}
}

func TestSaveClaudePublishesAndSaveCodexPreservesConfig(t *testing.T) {
	acct := config.Account{ID: "acct", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	codex := config.Account{ID: "codex", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	svc, data := testService(t, "https://not-called.invalid", acct, codex)
	home := homeFile(data, "alice", "s1")
	put(t, home, expiredJSON())
	raw := strings.ReplaceAll(expiredJSON(), "old-test-refresh", "saved-test-refresh")
	if err := svc.SaveClaude(t.Context(), acct.ID, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(home)
	if !strings.Contains(string(got), "saved-test-refresh") {
		t.Fatal("new login not published")
	}
	configPath := filepath.Join(codex.CredentialsDir, "config.toml")
	put(t, configPath, "model = \"custom-model\"\n")
	if err := svc.SaveCodexKey(t.Context(), codex.ID, "test-key", "https://relay.example.com/v1", "responses"); err != nil {
		t.Fatal(err)
	}
	if SavedAPIKey(codex.CredentialsDir) != "test-key" {
		t.Fatal("key not saved")
	}
	got, _ = os.ReadFile(configPath)
	if !strings.Contains(string(got), "custom-model") {
		t.Fatal("provider save lost unrelated config")
	}
	if base, wire := ReadCodexProvider(codex.CredentialsDir); base != "https://relay.example.com/v1" || wire != "responses" {
		t.Fatalf("provider %q %q", base, wire)
	}
}
