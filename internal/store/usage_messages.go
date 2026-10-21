package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// InsertUsageMessages settles independently identified Claude messages, one
// usage row per message (one provider API response). Claiming identities,
// writing rows and debiting credit are one transaction, so a replay after
// restart (even under a different turn ID) cannot charge twice. Each input has
// already been priced using the turn's pinned price table.
func (s *Store) InsertUsageMessages(messages ...UsageEvent) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var evs []UsageEvent
	for _, e := range messages {
		if e.SessionID == "" || e.ReqID == "" || e.TurnID == "" || e.Agent != "claude" || e.Kind != UsageKindChat && e.Kind != UsageKindSuggestion && e.Kind != UsageKindKeepalive || e.Price == nil {
			return 0, fmt.Errorf("invalid Claude message settlement")
		}
		// Validate even a duplicate: malformed snapshots must not be hidden.
		if _, err := priceJSON(e.Price); err != nil {
			return 0, err
		}
		if !json.Valid([]byte(e.Raw)) {
			return 0, fmt.Errorf("invalid message usage evidence")
		}
		res, err := tx.Exec(`INSERT INTO usage_messages(session_id,message_id,turn_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, e.SessionID, e.ReqID, e.TurnID)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		// The message ID lives in usage_messages and raw. req_id stays empty:
		// its partial unique index is the terminal scanner's upsert key.
		e.ReqID = ""
		evs = append(evs, e)
	}
	if err := insertUsageTx(tx, evs); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.usageWritten(evs)
	return len(evs), nil
}

// FinishUsageRows fills in what a web turn only learns as it ends on request
// rows settled while it still ran: the turn's own latency (TTFTMs, DurationMS,
// WallMS, on its first request), the provider name and a provider request id
// the stream did not carry yet. Tokens, cost and the ledger stay exactly as
// settled. Each row is the one its turn wrote with exactly the given Raw.
func (s *Store) FinishUsageRows(evs ...UsageEvent) error {
	if len(evs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range evs {
		if e.Kind == "" {
			e.Kind = UsageKindChat
		}
		if e.SessionID == "" || e.TurnID == "" || e.Raw == "" {
			return fmt.Errorf("invalid usage row to finish")
		}
		var id int64
		var requestID string
		err := tx.QueryRow(`SELECT id, api_request_id FROM usage_events
			WHERE session_id = ? AND turn_id = ? AND kind = ? AND raw = ? ORDER BY id LIMIT 1`,
			e.SessionID, e.TurnID, e.Kind, e.Raw).Scan(&id, &requestID)
		if err == sql.ErrNoRows {
			return fmt.Errorf("finish usage %s/%s: row not found", e.SessionID, e.TurnID)
		}
		if err != nil {
			return err
		}
		if requestID == "" && e.APIRequestID != "" {
			// Same as a fresh insert: per-request timing that arrived first
			// waits under the request id until a row names it.
			if err := claimRequestTimingTx(tx, &e); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE usage_events SET api_request_id = ?, api_ttft_ms = ?, api_duration_ms = ? WHERE id = ?`,
				e.APIRequestID, e.APITTFTMs, e.APIDurationMS, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE usage_events SET ttft_ms = ?, duration_ms = ?, wall_ms = ?,
			provider = CASE WHEN ? = '' THEN provider ELSE ? END WHERE id = ?`,
			e.TTFTMs, e.DurationMS, e.WallMS, e.Provider, e.Provider, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.usageWritten(evs)
	return nil
}
