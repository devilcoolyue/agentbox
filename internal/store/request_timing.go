package store

import (
	"database/sql"
	"fmt"
	"time"
)

// RequestTiming is what the CLI measured for one provider request: Claude
// Code's OpenTelemetry api_request event, matched to a usage row by the
// provider request id. The session is where the event came from; rows are
// matched across the user's sessions because a delegated worker's requests
// run in a hidden session but are recorded on its owner's.
type RequestTiming struct {
	User       string
	SessionID  string
	RequestID  string
	TTFTMs     int64
	DurationMS int64
}

const (
	// requestTimingKeep bounds how long telemetry waits for its usage row.
	// Rows land when a web turn ends or seconds after a terminal request is
	// written; what is still unmatched after this never gets a row (title and
	// compaction requests, CLI-internal calls).
	requestTimingKeep = 72 * time.Hour
	maxRequestIDLen   = 128
)

// requestTimingPerUser caps one user's unmatched backlog, so a process posting
// made-up ids cannot grow the database without bound. A variable for tests.
var requestTimingPerUser = 20000

// RecordRequestTimings stores CLI-measured timings. A usage row already on
// record takes its timing at once; otherwise the timing waits until the row
// is written (see claimRequestTimingTx). The first timing for a request wins:
// a later duplicate changes nothing. It reports how many usage rows changed.
func (s *Store) RecordRequestTimings(ts []RequestTiming) (int, error) {
	if len(ts) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now()
	if s.timingPruned.Load() < now.Add(-time.Hour).UnixNano() {
		if _, err := tx.Exec("DELETE FROM request_timings WHERE received_at < ?", usageTimestamp(now.Add(-requestTimingKeep))); err != nil {
			return 0, err
		}
		s.timingPruned.Store(now.UnixNano())
	}
	var touched []UsageEvent
	pending := map[string]int{} // waiting entries per user, counted once per batch
	for _, t := range ts {
		if t.User == "" || t.RequestID == "" || len(t.RequestID) > maxRequestIDLen || t.DurationMS <= 0 || t.TTFTMs < 0 || t.TTFTMs > t.DurationMS {
			continue
		}
		sessions, err := applyRequestTimingTx(tx, t)
		if err != nil {
			return 0, err
		}
		for _, id := range sessions {
			touched = append(touched, UsageEvent{User: t.User, SessionID: id})
		}
		if len(sessions) > 0 {
			continue
		}
		// A row that already has its timing (a resent batch) needs no entry.
		var timed int
		if err := tx.QueryRow("SELECT COUNT(*) FROM usage_events WHERE user=? AND api_request_id=? AND api_request_id != ''", t.User, t.RequestID).Scan(&timed); err != nil {
			return 0, err
		}
		if timed > 0 {
			continue
		}
		n, counted := pending[t.User]
		if !counted {
			if err := tx.QueryRow("SELECT COUNT(*) FROM request_timings WHERE user=?", t.User).Scan(&n); err != nil {
				return 0, err
			}
		}
		if n >= requestTimingPerUser {
			pending[t.User] = n
			continue
		}
		res, err := tx.Exec(`INSERT INTO request_timings(user,request_id,session_id,ttft_ms,duration_ms,received_at)
			VALUES(?,?,?,?,?,?) ON CONFLICT(user,request_id) DO NOTHING`,
			t.User, t.RequestID, t.SessionID, t.TTFTMs, t.DurationMS, usageTimestamp(now))
		if err != nil {
			return 0, fmt.Errorf("record request timing: %w", err)
		}
		added, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		pending[t.User] = n + int(added)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.usageWritten(touched)
	return len(touched), nil
}

// applyRequestTimingTx fills the timing into the user's rows for the request
// that do not have one yet and returns their sessions. The queries spell out
// the non-empty condition so SQLite can use the partial index: a bound value
// alone does not imply it.
func applyRequestTimingTx(tx *sql.Tx, t RequestTiming) ([]string, error) {
	rows, err := tx.Query(`UPDATE usage_events SET api_ttft_ms=?, api_duration_ms=?
		WHERE user=? AND api_request_id=? AND api_request_id != '' AND api_duration_ms=0 RETURNING session_id`,
		t.TTFTMs, t.DurationMS, t.User, t.RequestID)
	if err != nil {
		return nil, fmt.Errorf("apply request timing: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// claimRequestTimingTx gives a usage row about to be written the timing that
// arrived before it, and drops the waiting entry. Rows that already carry a
// timing, or name no request, are left alone.
func claimRequestTimingTx(tx *sql.Tx, e *UsageEvent) error {
	if e.APIRequestID == "" || e.User == "" || e.APIDurationMS > 0 {
		return nil
	}
	err := tx.QueryRow("SELECT ttft_ms, duration_ms FROM request_timings WHERE user=? AND request_id=?",
		e.User, e.APIRequestID).Scan(&e.APITTFTMs, &e.APIDurationMS)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim request timing: %w", err)
	}
	_, err = tx.Exec("DELETE FROM request_timings WHERE user=? AND request_id=?", e.User, e.APIRequestID)
	return err
}
