package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func switchFixture(t *testing.T) (*Service, *fakeRuntime, store.Session) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sess := store.Session{ID: "s1", User: "alice", Agent: config.AgentClaude, AccountID: "previous", ContainerID: "cid", Status: store.StatusRunning, ChatSession: "provider-thread"}
	if err := st.Put(sess); err != nil {
		t.Fatal(err)
	}
	accounts := map[string]config.Account{
		"previous": {ID: "previous", Type: config.AgentClaude},
		"next":     {ID: "next", Type: config.AgentClaude},
		"codex":    {ID: "codex", Type: config.AgentCodex},
	}
	runtime := &fakeRuntime{running: true}
	service := New(&config.Config{DataDir: root}, st, runtime, func(s store.Session) (config.Account, error) {
		a, ok := accounts[s.AccountID]
		if !ok {
			return config.Account{}, errors.New("account gone")
		}
		return a, nil
	}, func(context.Context, config.Account, store.Session) error { return nil })
	return service, runtime, sess
}

func TestSwitchAccountStopsBeforeHandoverAndKeepsConversation(t *testing.T) {
	s, r, sess := switchFixture(t)
	var released store.Session
	updated, err := s.SwitchAccount(t.Context(), sess.ID, "next", func(_ context.Context, cur store.Session, rebind func() error) error {
		if r.stops != 1 {
			t.Fatalf("handover ran before the container stopped (stops=%d)", r.stops)
		}
		released = cur
		return rebind()
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.AccountID != "previous" || released.Status != store.StatusStopped {
		t.Fatalf("handover got %+v, want the stopped workspace on its previous account", released)
	}
	if updated.AccountID != "next" || updated.Status != store.StatusStopped || updated.ChatSession != "provider-thread" {
		t.Fatalf("updated = %+v", updated)
	}
	if cur, _ := s.store.Get(sess.ID); cur.AccountID != "next" {
		t.Fatalf("stored account = %q", cur.AccountID)
	}
}

func TestSwitchAccountRejectsAndFailsWithoutRebinding(t *testing.T) {
	s, r, sess := switchFixture(t)
	never := func(context.Context, store.Session, func() error) error {
		t.Fatal("handover must not run")
		return nil
	}
	if _, err := s.SwitchAccount(t.Context(), sess.ID, "codex", never); !errors.Is(err, ErrAccountAgent) {
		t.Fatalf("cross-agent switch: %v", err)
	}
	if _, err := s.SwitchAccount(t.Context(), sess.ID, "missing", never); err == nil {
		t.Fatal("switched to an unknown account")
	}
	if got, err := s.SwitchAccount(t.Context(), sess.ID, "previous", never); err != nil || got.Status != store.StatusRunning {
		t.Fatalf("same account must be a no-op: %+v %v", got, err)
	}
	if r.stops != 0 {
		t.Fatalf("rejected switches stopped the container %d times", r.stops)
	}

	r.fail = errors.New("Docker unavailable")
	if _, err := s.SwitchAccount(t.Context(), sess.ID, "next", never); err == nil {
		t.Fatal("stop failure swallowed")
	}
	r.fail = nil
	failed := errors.New("credentials unavailable")
	if _, err := s.SwitchAccount(t.Context(), sess.ID, "next", func(context.Context, store.Session, func() error) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("handover failure: %v", err)
	}
	cur, _ := s.store.Get(sess.ID)
	if cur.AccountID != "previous" || cur.Status != store.StatusStopped {
		t.Fatalf("after failed handover = %+v, want previous account and recorded stop", cur)
	}
	if _, err := s.SwitchAccount(t.Context(), "gone", "next", never); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("missing workspace: %v", err)
	}
}
