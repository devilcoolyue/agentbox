package store

import (
	"encoding/json"
	"strings"
)

// ChatTurnCost sums committed chat rows only. It never re-prices history using
// today's settings or includes title/terminal spend in an answer's cost.
type ChatTurnCost struct {
	TurnID       string `json:"turn_id"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
	Source       string `json:"source"` // provider | table | mixed | unpriced | unknown
	Partial      bool   `json:"partial,omitempty"`
}

func (s *Store) ChatTurnCosts(sessionID, threadID string, turnIDs []string) (map[string]ChatTurnCost, error) {
	out := make(map[string]ChatTurnCost)
	seen := make(map[string]bool)
	ids := make([]string, 0, len(turnIDs))
	for _, id := range turnIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		args := []any{sessionID, threadID, UsageKindChat}
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := s.db.Query(`SELECT turn_id, cost_micro_usd, agent, price_snapshot FROM usage_events
			WHERE session_id = ? AND thread_id = ? AND kind = ? AND turn_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, agent, raw string
			var cost int64
			if err := rows.Scan(&id, &cost, &agent, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			source := "unpriced"
			if raw != "" {
				var price PriceSnapshot
				if err := json.Unmarshal([]byte(raw), &price); err != nil {
					rows.Close()
					return nil, err
				}
				source = price.Source
			} else if agent == "claude" && cost > 0 {
				source = "provider"
			} else if cost > 0 {
				source = "table"
			}
			v, exists := out[id]
			v.TurnID = id
			v.CostMicroUSD += cost
			if !exists {
				v.Source = source
			} else if v.Source != source {
				v.Source = "mixed"
			}
			v.Partial = v.Partial || source == "unpriced"
			out[id] = v
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
