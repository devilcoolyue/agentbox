package store

import (
	"fmt"
	"time"
)

// InsertBackgroundUsage records Codex's background requests (UsageKindBackground)
// as reported by its telemetry, and charges the new ones like terminal rows
// (settleUsageTx). ReqID keeps a resent export from adding — or charging — the
// same request twice; it returns how many rows were new.
func (s *Store) InsertBackgroundUsage(evs ...UsageEvent) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var added []UsageEvent
	metered := map[string]bool{}
	for _, e := range evs {
		if e.ReqID == "" {
			return 0, fmt.Errorf("background usage %s: 缺 req_id", e.SessionID)
		}
		if e.TS.IsZero() {
			e.TS = time.Now()
		}
		snapshot, err := priceJSON(e.Price)
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(`INSERT INTO usage_events
			(ts, user, session_id, thread_id, turn_id, agent, account_id, model, kind,
			 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			 cost_micro_usd, duration_ms, wall_ms, ttft_ms, provider, req_id, raw, price_snapshot, effort, effort_control,
			 api_request_id, api_ttft_ms, api_duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(req_id) WHERE req_id != '' DO NOTHING`,
			usageTimestamp(e.TS), e.User, e.SessionID, e.ThreadID, e.TurnID,
			e.Agent, e.AccountID, e.Model, UsageKindBackground,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens,
			e.CostMicroUSD, e.DurationMS, e.WallMS, e.TTFTMs, e.Provider, e.ReqID, e.Raw, snapshot, e.Effort, e.EffortControl,
			e.APIRequestID, e.APITTFTMs, e.APIDurationMS)
		if err != nil {
			return 0, fmt.Errorf("insert background usage %s/%s: %w", e.SessionID, e.ReqID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if n == 0 {
			continue
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if err := settleUsageTx(tx, id, e.User, usageTimestamp(e.TS), settleNote(UsageKindBackground, e), e.CostMicroUSD, metered); err != nil {
			return 0, err
		}
		added = append(added, e)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.usageWritten(added)
	return len(added), nil
}
