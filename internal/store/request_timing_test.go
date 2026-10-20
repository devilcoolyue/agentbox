package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openTimingStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func usageByReq(t *testing.T, s *Store, apiReq string) UsageEvent {
	t.Helper()
	for _, e := range s.ListUsage(UsageFilter{}) {
		if e.APIRequestID == apiReq {
			return e
		}
	}
	t.Fatalf("no usage row for %s", apiReq)
	return UsageEvent{}
}

func pendingTimings(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM request_timings").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Telemetry is exported every few seconds and on exit, so it reaches the
// server before a web turn's rows (written when the turn ends) and on either
// side of a terminal row. Both orders end with the same row.
func TestRequestTimingEitherOrder(t *testing.T) {
	s := openTimingStore(t)
	var notified []UsageWrite
	s.OnUsageWritten(func(w []UsageWrite) { notified = append(notified, w...) })
	row := func(id string) UsageEvent {
		return UsageEvent{User: "alice", SessionID: "s1", TurnID: "t1", Agent: "claude", Model: "claude-opus-5-5",
			OutputTokens: 900, APIRequestID: id, TS: time.Now()}
	}

	// Telemetry first: it waits, then the inserted row claims it.
	if n, err := s.RecordRequestTimings([]RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_early", TTFTMs: 1500, DurationMS: 12700}}); err != nil || n != 0 {
		t.Fatalf("record early = %d, %v", n, err)
	}
	if pendingTimings(t, s) != 1 {
		t.Fatal("early timing should wait for its row")
	}
	if err := s.InsertUsage(row("req_early")); err != nil {
		t.Fatal(err)
	}
	if e := usageByReq(t, s, "req_early"); e.APITTFTMs != 1500 || e.APIDurationMS != 12700 {
		t.Fatalf("claimed timing = %d/%d", e.APITTFTMs, e.APIDurationMS)
	}
	if pendingTimings(t, s) != 0 {
		t.Fatal("a claimed timing must not linger")
	}

	// Row first: the timing lands on it directly and pushes a refresh.
	if err := s.InsertUsage(row("req_late")); err != nil {
		t.Fatal(err)
	}
	notified = nil
	if n, err := s.RecordRequestTimings([]RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_late", TTFTMs: 400, DurationMS: 2000}}); err != nil || n != 1 {
		t.Fatalf("record late = %d, %v", n, err)
	}
	if e := usageByReq(t, s, "req_late"); e.APITTFTMs != 400 || e.APIDurationMS != 2000 {
		t.Fatalf("applied timing = %d/%d", e.APITTFTMs, e.APIDurationMS)
	}
	if len(notified) != 1 || notified[0] != (UsageWrite{User: "alice", SessionID: "s1"}) {
		t.Fatalf("notified %+v", notified)
	}
	// A resent batch changes nothing and leaves nothing behind.
	if n, err := s.RecordRequestTimings([]RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_late", TTFTMs: 1, DurationMS: 9}}); err != nil || n != 0 {
		t.Fatalf("resend = %d, %v", n, err)
	}
	if e := usageByReq(t, s, "req_late"); e.APIDurationMS != 2000 || pendingTimings(t, s) != 0 {
		t.Fatalf("resend changed the row (%d) or queued (%d)", e.APIDurationMS, pendingTimings(t, s))
	}
}

// The terminal scanner re-upserts the same request every sweep without a
// timing of its own; a measured timing must survive that, and a timing that
// arrived first is claimed by whichever upsert comes next.
func TestRequestTimingSurvivesTerminalUpserts(t *testing.T) {
	s := openTimingStore(t)
	term := UsageEvent{User: "alice", SessionID: "s1", TurnID: "req_a", Agent: "claude", Model: "claude-opus-5-5",
		OutputTokens: 10, ReqID: "req_a", APIRequestID: "req_a", WallMS: 2400, TTFTMs: 0, TS: time.Now()}
	if err := s.UpsertTerminalUsage(term); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordRequestTimings([]RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_a", TTFTMs: 1900, DurationMS: 2450}}); err != nil {
		t.Fatal(err)
	}
	term.OutputTokens, term.WallMS = 120, 2420
	if err := s.UpsertTerminalUsage(term); err != nil {
		t.Fatal(err)
	}
	if e := usageByReq(t, s, "req_a"); e.APITTFTMs != 1900 || e.APIDurationMS != 2450 || e.OutputTokens != 120 || e.WallMS != 2420 {
		t.Fatalf("after rescan: %+v", e)
	}

	if _, err := s.RecordRequestTimings([]RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_b", TTFTMs: 700, DurationMS: 3000}}); err != nil {
		t.Fatal(err)
	}
	next := term
	next.ReqID, next.APIRequestID = "req_b", "req_b"
	if err := s.UpsertTerminalUsage(next); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTerminalUsage(next); err != nil {
		t.Fatal(err)
	}
	if e := usageByReq(t, s, "req_b"); e.APITTFTMs != 700 || e.APIDurationMS != 3000 {
		t.Fatalf("claimed by upsert: %d/%d", e.APITTFTMs, e.APIDurationMS)
	}
}

// Telemetry is scoped to the user whose session sent it: another user's
// process cannot set timings on rows it does not own, even with the right id.
func TestRequestTimingScopedToUser(t *testing.T) {
	s := openTimingStore(t)
	if err := s.InsertUsage(UsageEvent{User: "alice", SessionID: "s1", TurnID: "t", Agent: "claude", APIRequestID: "req_x", TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RecordRequestTimings([]RequestTiming{{User: "mallory", SessionID: "s9", RequestID: "req_x", TTFTMs: 1, DurationMS: 2}}); err != nil || n != 0 {
		t.Fatalf("cross-user record = %d, %v", n, err)
	}
	if e := usageByReq(t, s, "req_x"); e.APIDurationMS != 0 {
		t.Fatalf("another user's telemetry landed: %+v", e)
	}
	// Implausible values are dropped rather than stored.
	for _, bad := range []RequestTiming{
		{User: "alice", RequestID: "req_x", TTFTMs: 5, DurationMS: 0},
		{User: "alice", RequestID: "req_x", TTFTMs: 50, DurationMS: 40},
		{User: "alice", RequestID: "req_x", TTFTMs: -1, DurationMS: 40},
		{User: "", RequestID: "req_x", TTFTMs: 1, DurationMS: 40},
	} {
		if n, err := s.RecordRequestTimings([]RequestTiming{bad}); err != nil || n != 0 {
			t.Fatalf("%+v = %d, %v", bad, n, err)
		}
	}
	if pendingTimings(t, s) != 1 { // only mallory's well-formed entry waits
		t.Fatalf("pending = %d", pendingTimings(t, s))
	}
	if err := s.DeleteUser("mallory"); err != nil {
		t.Fatal(err)
	}
	if pendingTimings(t, s) != 0 {
		t.Fatal("deleting a user must drop their waiting timings")
	}
}

func TestRequestTimingBacklogBounded(t *testing.T) {
	s := openTimingStore(t)
	defer func(n int) { requestTimingPerUser = n }(requestTimingPerUser)
	requestTimingPerUser = 50
	batch := make([]RequestTiming, 0, requestTimingPerUser+5)
	for i := range requestTimingPerUser + 5 {
		batch = append(batch, RequestTiming{User: "alice", SessionID: "s1", RequestID: fmt.Sprintf("req_%d", i), TTFTMs: 1, DurationMS: 2})
	}
	if _, err := s.RecordRequestTimings(batch); err != nil {
		t.Fatal(err)
	}
	if n := pendingTimings(t, s); n != requestTimingPerUser {
		t.Fatalf("pending = %d, want the cap %d", n, requestTimingPerUser)
	}
	// Stale entries are swept on the next write.
	if _, err := s.db.Exec("UPDATE request_timings SET received_at=?", usageTimestamp(time.Now().Add(-requestTimingKeep-time.Hour))); err != nil {
		t.Fatal(err)
	}
	s.timingPruned.Store(0)
	if _, err := s.RecordRequestTimings([]RequestTiming{{User: "alice", SessionID: "s1", RequestID: "req_new", TTFTMs: 1, DurationMS: 2}}); err != nil {
		t.Fatal(err)
	}
	if n := pendingTimings(t, s); n != 1 {
		t.Fatalf("after sweep pending = %d", n)
	}
}
