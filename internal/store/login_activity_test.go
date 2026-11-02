package store

import (
	"path/filepath"
	"testing"
	"time"
)

func loginActivityStore(t *testing.T, users ...string) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, name := range users {
		if err := st.CreateUser(User{Name: name, Role: RoleUser, PassHash: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestLoginEventsPageNewestFirstAndFilter(t *testing.T) {
	st := loginActivityStore(t, "alice", "bob")
	for i, user := range []string{"alice", "bob", "alice", "alice"} {
		if err := st.RecordLogin(LoginEvent{User: user, At: int64(i + 1), Method: LoginPassword, IP: "203.0.113.1", UserAgent: "ua"}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.LoginEvents("", 0, 10)
	if err != nil || len(all) != 4 || all[0].At != 4 || all[3].At != 1 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if all[0].User != "alice" || all[0].Method != LoginPassword || all[0].IP != "203.0.113.1" || all[0].UserAgent != "ua" {
		t.Fatalf("fields lost: %+v", all[0])
	}
	first, _ := st.LoginEvents("alice", 0, 2)
	if len(first) != 2 || first[0].At != 4 || first[1].At != 3 {
		t.Fatalf("first page = %+v", first)
	}
	rest, _ := st.LoginEvents("alice", first[1].ID, 2)
	if len(rest) != 1 || rest[0].At != 1 {
		t.Fatalf("next page = %+v", rest)
	}
	if none, _ := st.LoginEvents("nobody", 0, 10); len(none) != 0 {
		t.Fatalf("unknown user = %+v", none)
	}
}

func TestLoginEventsPurgeOnlyPastRetention(t *testing.T) {
	st := loginActivityStore(t, "alice")
	now := time.Now()
	for _, at := range []time.Time{now.Add(-loginEventKeep - time.Hour), now.Add(-loginEventKeep + time.Hour)} {
		if err := st.RecordLogin(LoginEvent{User: "alice", At: at.UnixMilli(), Method: LoginPassword}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.PurgeLoginEvents(now); err != nil || n != 1 {
		t.Fatalf("purged = %d, %v", n, err)
	}
	if left, _ := st.LoginEvents("", 0, 10); len(left) != 1 || left[0].At != now.Add(-loginEventKeep+time.Hour).UnixMilli() {
		t.Fatalf("left = %+v", left)
	}
}

func TestUserActivityListsEveryAccountMostRecentFirst(t *testing.T) {
	st := loginActivityStore(t, "alice", "bob", "carol")
	now := time.UnixMilli(1_800_000_000_000)
	if err := st.TouchUserActivity("bob", now); err != nil {
		t.Fatal(err)
	}
	// Tabs report independently, so one can arrive late: it must not rewind.
	if err := st.TouchUserActivity("bob", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordLogin(LoginEvent{User: "alice", At: 10, Method: LoginPassword, IP: "203.0.113.1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordLogin(LoginEvent{User: "alice", At: 20, Method: LoginPassword, IP: "203.0.113.2"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListUserActivity()
	if err != nil || len(got) != 3 {
		t.Fatalf("got = %+v, %v", got, err)
	}
	if got[0].Name != "bob" || got[0].LastActiveAt != now.UnixMilli() || got[0].LastLoginAt != 0 {
		t.Fatalf("active user = %+v", got[0])
	}
	if got[1].Name != "alice" || got[1].LastActiveAt != 0 || got[1].LastLoginAt != 20 || got[1].LastLoginIP != "203.0.113.2" {
		t.Fatalf("logged-in user shows the latest login: %+v", got[1])
	}
	if got[2].Name != "carol" || got[2].LastActiveAt != 0 || got[2].LastLoginAt != 0 {
		t.Fatalf("idle user = %+v", got[2])
	}
}

// A same-named account created later must not inherit the old one's history,
// and a write finishing after the delete must not leave a row behind.
func TestDeletedUserLeavesNoLoginOrActivityRows(t *testing.T) {
	st := loginActivityStore(t, "alice")
	if err := st.RecordLogin(LoginEvent{User: "alice", At: 1, Method: LoginPassword}); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchUserActivity("alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordLogin(LoginEvent{User: "alice", At: 2, Method: LoginPassword}); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchUserActivity("alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(User{Name: "alice", Role: RoleUser, PassHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.LoginEvents("alice", 0, 10); len(left) != 0 {
		t.Fatalf("login events survived: %+v", left)
	}
	got, _ := st.ListUserActivity()
	if len(got) != 1 || got[0].LastActiveAt != 0 || got[0].LastLoginAt != 0 {
		t.Fatalf("recreated account inherited history: %+v", got)
	}
}
