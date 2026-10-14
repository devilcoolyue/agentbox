package store

import (
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
		if e.SessionID == "" || e.ReqID == "" || e.TurnID == "" || e.Agent != "claude" || e.Kind != UsageKindChat && e.Kind != UsageKindSuggestion || e.Price == nil {
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
	return len(evs), nil
}
