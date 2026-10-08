package credentials

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

// boundSessions lets rebind move the workspace between accounts mid-test.
type boundSessions struct {
	mu       sync.Mutex
	sessions []store.Session
}

func (b *boundSessions) All() []store.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]store.Session(nil), b.sessions...)
}
func (b *boundSessions) GetUser(name string) (store.User, bool) {
	return store.User{Name: name, Role: store.RoleUser}, true
}
func (b *boundSessions) rebind(account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[0].AccountID = account
}

func handoverFixture(t *testing.T, previousAccess *config.AccountAccess) (*Service, *boundSessions, config.Account, config.Account, string) {
	t.Helper()
	previous := config.Account{ID: "previous", Type: config.AgentClaude, CredentialsDir: t.TempDir(), Access: previousAccess}
	next := config.Account{ID: "next", Type: config.AgentClaude, CredentialsDir: t.TempDir()}
	sessions := &boundSessions{sessions: []store.Session{{ID: "s1", User: "alice", Agent: config.AgentClaude, AccountID: previous.ID}}}
	data := t.TempDir()
	svc := New(&config.Config{DataDir: data, Accounts: []config.Account{previous, next}}, sessions,
		func(_ config.Account, d time.Duration) (*http.Client, error) { return &http.Client{Timeout: d}, nil }, "test-public-client", func() string { return "" })
	old := time.Now().Add(-time.Hour)
	put(t, filepath.Join(previous.CredentialsDir, ".credentials.json"), `{"claudeAiOauth":{"refreshToken":"previous-pool-refresh"}}`)
	put(t, filepath.Join(next.CredentialsDir, ".credentials.json"), `{"claudeAiOauth":{"refreshToken":"next-pool-refresh"}}`)
	for _, p := range []string{filepath.Join(previous.CredentialsDir, ".credentials.json"), filepath.Join(next.CredentialsDir, ".credentials.json")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// The workspace's CLI refreshed the previous account after the pool's copy.
	put(t, homeFile(data, "alice", "s1"), `{"claudeAiOauth":{"refreshToken":"previous-rotated-refresh"}}`)
	return svc, sessions, previous, next, data
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReleaseReturnsRotatedTokenAndNeverFeedsNextAccount(t *testing.T) {
	svc, sessions, previous, next, data := handoverFixture(t, nil)
	sess := sessions.All()[0]
	if err := svc.Release(t.Context(), sess, func() error { sessions.rebind(next.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(previous.CredentialsDir, ".credentials.json")); got != `{"claudeAiOauth":{"refreshToken":"previous-rotated-refresh"}}` {
		t.Fatalf("rotated token not returned to the previous pool: %s", got)
	}
	if _, err := os.Stat(homeFile(data, "alice", "s1")); !os.IsNotExist(err) {
		t.Fatalf("previous login left in the home: %v", err)
	}
	// Ordinary sync and broadcast for the next account must not adopt anything.
	bound := sessions.All()[0]
	if err := svc.Sync(t.Context(), next, bound); err != nil {
		t.Fatal(err)
	}
	svc.SyncAll(t.Context())
	svc.broadcast(next)
	if got := readText(t, filepath.Join(next.CredentialsDir, ".credentials.json")); got != `{"claudeAiOauth":{"refreshToken":"next-pool-refresh"}}` {
		t.Fatalf("next pool changed by the handover: %s", got)
	}
	if _, err := os.Stat(homeFile(data, "alice", "s1")); !os.IsNotExist(err) {
		t.Fatalf("a sync re-created a login before seeding: %v", err)
	}
}

func TestReleaseSkipsReturnForRevokedWorkspaceAndKeepsAccountOnFailure(t *testing.T) {
	svc, sessions, previous, next, data := handoverFixture(t, &config.AccountAccess{Mode: "admin"})
	sess := sessions.All()[0]
	failed := errors.New("store unavailable")
	if err := svc.Release(t.Context(), sess, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("rebind error not returned: %v", err)
	}
	// A revoked workspace never writes into the pool, same as ordinary sync.
	if got := readText(t, filepath.Join(previous.CredentialsDir, ".credentials.json")); got != `{"claudeAiOauth":{"refreshToken":"previous-pool-refresh"}}` {
		t.Fatalf("revoked workspace wrote the previous pool: %s", got)
	}
	if _, err := os.Stat(homeFile(data, "alice", "s1")); !os.IsNotExist(err) {
		t.Fatalf("login kept after handover: %v", err)
	}
	if got := readText(t, filepath.Join(next.CredentialsDir, ".credentials.json")); got != `{"claudeAiOauth":{"refreshToken":"next-pool-refresh"}}` {
		t.Fatalf("next pool changed: %s", got)
	}
}
