package store

import (
	"fmt"
	"time"
)

// UsageSlotSize is the width of the slots UsageSlots sums into.
const UsageSlotSize = 15 * time.Minute

// UsageSlot is every matching row of one UTC quarter-hour summed up.
type UsageSlot struct {
	Start            time.Time
	Requests         int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicroUSD     int64
}

// UsageSlots sums the rows a filter matches per UTC quarter-hour, oldest first.
// Limit/Offset/Asc are ignored: it describes the filter, like SumUsage.
//
// SQLite cannot group by a configured IANA zone, and reading every row into Go
// would make each refresh scale with the table. Timezone offsets in use are all
// multiples of 15 minutes, so a quarter-hour never straddles a local hour or day
// and the caller can fold these slots into any local bucket. It relies on ts
// being the fixed-width text written by usageTimestamp.
func (s *Store) UsageSlots(f UsageFilter) ([]UsageSlot, error) {
	where, args := f.where()
	rows, err := s.db.Query(`SELECT
		substr(ts,1,14) || printf('%02d', CAST(substr(ts,15,2) AS INTEGER) / 15 * 15) AS slot,
		COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(cost_micro_usd),0)
		FROM usage_events`+where+` GROUP BY slot ORDER BY slot`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageSlot
	for rows.Next() {
		var slot string
		var u UsageSlot
		if err := rows.Scan(&slot, &u.Requests, &u.InputTokens, &u.OutputTokens,
			&u.CacheReadTokens, &u.CacheWriteTokens, &u.CostMicroUSD); err != nil {
			return nil, err
		}
		if u.Start, err = time.Parse("2006-01-02T15:04", slot); err != nil {
			return nil, fmt.Errorf("usage slot %q: %w", slot, err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
