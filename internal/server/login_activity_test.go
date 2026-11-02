package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
)

func loginRequest(handler http.Handler, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": "alice", "password": password})
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(string(body)))
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	r.Header.Set("User-Agent", "Fixture\x00Browser/1.0")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestPasswordLoginIsRecordedOnlyWhenItSucceeds(t *testing.T) {
	s, handler := pairingFixture(t)
	if w := loginRequest(handler, "synthetic-wrong-password"); w.Code == http.StatusOK {
		t.Fatal("wrong password logged in")
	}
	if events, _ := s.store.LoginEvents("", 0, 10); len(events) != 0 {
		t.Fatalf("failed login recorded: %+v", events)
	}
	if w := loginRequest(handler, "synthetic-old-password"); w.Code != http.StatusOK {
		t.Fatal("login status", w.Code)
	}
	events, err := s.store.LoginEvents("", 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	e := events[0]
	// The address is the hop the proxy appended, not the one the client claimed.
	if e.User != "alice" || e.Method != store.LoginPassword || e.IP != "203.0.113.9" || e.UserAgent != "FixtureBrowser/1.0" {
		t.Fatalf("event = %+v", e)
	}
	if time.Since(time.UnixMilli(e.At)) > time.Minute {
		t.Fatalf("event time = %d", e.At)
	}
}

func TestPairingRedemptionIsRecordedAsItsOwnMethod(t *testing.T) {
	for channel, method := range map[string]string{"desktop": store.LoginDesktop, "tunnel": store.LoginLink} {
		t.Run(channel, func(t *testing.T) {
			s, handler := pairingFixture(t)
			_, redeem := pairRoutes(channel)
			code := issueFixturePair(t, handler, channel)
			if w := clientRequest(handler, "", "POST", redeem, `{"code":"not-a-code"}`); w.Code == http.StatusOK {
				t.Fatal("invalid code redeemed")
			}
			if events, _ := s.store.LoginEvents("", 0, 10); len(events) != 0 {
				t.Fatalf("failed redemption recorded: %+v", events)
			}
			if w := clientRequest(handler, "", "POST", redeem, `{"code":"`+code+`"}`); w.Code != http.StatusOK {
				t.Fatal("redeem status", w.Code, w.Body.String())
			}
			events, _ := s.store.LoginEvents("", 0, 10)
			if len(events) != 1 || events[0].User != "alice" || events[0].Method != method {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

// The whole point of the panel being admin-only: a user can't read this about
// anyone, themselves included.
func TestLoginAndActivityRecordsAreAdminOnly(t *testing.T) {
	s, handler := pairingFixture(t)
	if err := s.store.RecordLogin(store.LoginEvent{User: "alice", At: 1, Method: store.LoginPassword, IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/user-activity", "/api/login-events", "/api/login-events?user=alice"} {
		if w := clientRequest(handler, "", "GET", route, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a login = %d", route, w.Code)
		}
		w := clientRequest(handler, "alice", "GET", route, "")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s as a user = %d", route, w.Code)
		}
		if strings.Contains(w.Body.String(), "203.0.113.9") {
			t.Errorf("%s as a user leaked the record: %s", route, w.Body)
		}
		w = clientRequest(handler, "root", "GET", route, "")
		if w.Code != http.StatusOK {
			t.Errorf("%s as admin = %d: %s", route, w.Code, w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s may be cached", route)
		}
	}
	// A user's own profile must not carry it either.
	if w := clientRequest(handler, "alice", "GET", "/api/me", ""); strings.Contains(w.Body.String(), "203.0.113.9") || strings.Contains(w.Body.String(), "last_active") {
		t.Errorf("/api/me exposes activity: %s", w.Body)
	}
}

func TestLoginEventsPagination(t *testing.T) {
	s, handler := pairingFixture(t)
	for i := range 3 {
		if err := s.store.RecordLogin(store.LoginEvent{User: "alice", At: int64(i + 1), Method: store.LoginPassword}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(query string) (out struct {
		Events  []store.LoginEvent `json:"events"`
		HasMore bool               `json:"has_more"`
	}) {
		t.Helper()
		w := clientRequest(handler, "root", "GET", "/api/login-events"+query, "")
		if w.Code != http.StatusOK {
			t.Fatal(query, w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := read("?limit=2")
	if len(first.Events) != 2 || !first.HasMore || first.Events[0].At != 3 {
		t.Fatalf("first = %+v", first)
	}
	rest := read("?limit=2&before=" + strconv.FormatInt(first.Events[1].ID, 10))
	if len(rest.Events) != 1 || rest.HasMore || rest.Events[0].At != 1 {
		t.Fatalf("rest = %+v", rest)
	}
	for _, bad := range []string{"?limit=0", "?limit=201", "?limit=x", "?before=0", "?before=x"} {
		if w := clientRequest(handler, "root", "GET", "/api/login-events"+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", bad, w.Code)
		}
	}
}

func TestLoginUserAgentIsBoundedAndPrintable(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.Header.Set("User-Agent", "a\x1b[31m\r\n\xffb"+strings.Repeat("字", 400))
	got := loginUserAgent(r)
	if !strings.HasPrefix(got, "a[31mb字") {
		t.Fatalf("control bytes survived: %q", got[:12])
	}
	if n := len([]rune(got)); n != loginUserAgentMaxRunes {
		t.Fatalf("length = %d", n)
	}
}

func lastActiveOf(t *testing.T, s *Server, name string) int64 {
	t.Helper()
	users, err := s.store.ListUserActivity()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Name == name {
			return u.LastActiveAt
		}
	}
	t.Fatalf("%s not listed", name)
	return 0
}

// A report moves only the sender's own time and gives nothing back: the data is
// for administrators, not for whoever is reporting.
func TestActivityReportRecordsTheTokenOwnerAndAnswersNothing(t *testing.T) {
	s, handler := pairingFixture(t)
	if w := clientRequest(handler, "", "POST", "/api/me/activity", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("without a login:", w.Code)
	}
	if got := lastActiveOf(t, s, "alice"); got != 0 {
		t.Fatalf("anonymous report recorded: %d", got)
	}
	before := time.Now().UnixMilli()
	w := clientRequest(handler, "alice", "POST", "/api/me/activity", `{"user":"bob"}`)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("report = %d %q", w.Code, w.Body)
	}
	if got := lastActiveOf(t, s, "alice"); got < before {
		t.Fatalf("alice = %d, want at least %d", got, before)
	}
	if got := lastActiveOf(t, s, "bob"); got != 0 {
		t.Fatalf("the body named another user and moved their time: %d", got)
	}
}

// A page spamming reports costs one write per gap, not one per request, and one
// user's reports don't hold back another's.
func TestActivityGateLimitsWritesPerUser(t *testing.T) {
	var g activityGate
	start := time.Now()
	for _, c := range []struct {
		user  string
		after time.Duration
		want  bool
	}{
		{"alice", 0, true},
		{"alice", time.Second, false},
		{"bob", time.Second, true},
		{"alice", activityMinGap - time.Millisecond, false},
		{"alice", activityMinGap, true},
		{"alice", activityMinGap + time.Second, false},
	} {
		if got := g.allow(c.user, start.Add(c.after)); got != c.want {
			t.Errorf("%s at +%v: allow = %v", c.user, c.after, got)
		}
	}
}
