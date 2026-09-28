package store

import (
	"encoding/json"
	"fmt"
)

// InsertUsageMessages settles independently identified Claude messages. Claiming
// identities, aggregating new messages and debiting credit are one transaction.
// A replay after restart (even under a different turn ID) cannot charge twice.
// Each input has already been priced using the turn's pinned price table.
func (s *Store) InsertUsageMessages(messages ...UsageEvent) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	type key struct{ user, session, thread, turn, model string }
	groups := map[key]int{}
	var evs []UsageEvent
	var raws [][]json.RawMessage
	for _, e := range messages {
		if e.SessionID == "" || e.ReqID == "" || e.TurnID == "" || e.Agent != "claude" || e.Kind != UsageKindChat || e.Price == nil {
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
		k := key{e.User, e.SessionID, e.ThreadID, e.TurnID, e.Model}
		i, ok := groups[k]
		if !ok {
			i = len(evs)
			groups[k] = i
			row := e
			row.ReqID, row.Raw = "", ""
			row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens, row.CostMicroUSD = 0, 0, 0, 0, 0
			evs = append(evs, row)
			raws = append(raws, nil)
		}
		row := &evs[i]
		row.InputTokens += e.InputTokens
		row.OutputTokens += e.OutputTokens
		row.CacheReadTokens += e.CacheReadTokens
		row.CacheWriteTokens += e.CacheWriteTokens
		row.CostMicroUSD += e.CostMicroUSD
		raws[i] = append(raws[i], json.RawMessage(e.Raw))
	}
	for i := range evs {
		raw, err := json.Marshal(struct {
			Type     string            `json:"type"`
			Messages []json.RawMessage `json:"messages"`
		}{"claude.messages", raws[i]})
		if err != nil {
			return 0, err
		}
		evs[i].Raw = string(raw)
	}
	if err := insertUsageTx(tx, evs); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(evs), nil
}
