package store

import "time"

type UsageModel struct{ Agent, Model string }

// Bounded discovery for the admin pricing page; the date filter uses the usage
// time index. Include blank models so unknown model/fallback usage stays visible.
func (s *Store) RecentUsageModels(since time.Time, limit int) ([]UsageModel, bool, error) {
	rows, err := s.db.Query(`SELECT agent, model FROM usage_events WHERE ts >= ? GROUP BY agent, model ORDER BY agent, model LIMIT ?`, usageTimestamp(since), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []UsageModel{}
	for rows.Next() {
		var m UsageModel
		if err := rows.Scan(&m.Agent, &m.Model); err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
