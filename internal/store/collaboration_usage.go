package store

import (
	"database/sql"
	"encoding/json"
	"math"
)

// CollaborationStepUsage is a read-only projection of the existing ledger.
// InputTokens excludes cache reads/writes; all four token categories are disjoint.
// Null is deliberately different from an observed zero. A partial row contains
// only recorded usage, never an estimate of the still-running or interrupted CLI.
type CollaborationStepUsage struct {
	StepID           string   `json:"step_id"`
	RunID            string   `json:"run_id"`
	TurnID           string   `json:"turn_id"`
	Kind             string   `json:"kind"`
	Role             string   `json:"role"`
	State            string   `json:"state"`
	Status           string   `json:"status"` // pending | settled | partial | unavailable
	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	CacheReadTokens  *int64   `json:"cache_read_tokens"`
	CacheWriteTokens *int64   `json:"cache_write_tokens"`
	TotalTokens      *int64   `json:"total_tokens"`
	CostMicroUSD     *int64   `json:"cost_micro_usd"`
	Models           []string `json:"models"`
}

// CollaborationUsage reads totals and per-step evidence from the same SQLite
// snapshot. It does not write usage, reprice old turns, or query a provider.
func (s *Store) CollaborationUsage(u User, run string) (CollaborationCost, []CollaborationStepUsage, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationCost{}, nil, err
	}
	defer tx.Rollback()
	if _, err = collaborationRunTx(tx, u, run); err != nil {
		return CollaborationCost{}, nil, err
	}
	cost, err := collaborationCostTx(tx, run)
	if err != nil {
		return cost, nil, err
	}
	steps, err := collaborationStepUsageTx(tx, u, run)
	return cost, steps, err
}

func collaborationStepUsageTx(tx *sql.Tx, u User, run string) ([]CollaborationStepUsage, error) {
	// Join the original participant generation, not the current role binding.
	// usage_events already contains deduplicated Claude message aggregates; joining
	// usage_messages or trace events here would multiply a turn's accounting rows.
	rows, err := tx.Query(`SELECT st.id,st.run_id,st.turn_id,st.kind,st.role,st.state,
		ue.id,ue.input_tokens,ue.output_tokens,ue.cache_read_tokens,ue.cache_write_tokens,
		ue.cost_micro_usd,ue.price_snapshot,ue.model
		FROM collaboration_steps st
		LEFT JOIN collaboration_participants p ON p.run_id=st.run_id AND p.role=st.role AND p.generation=st.participant_generation
		LEFT JOIN usage_events ue ON ue.turn_id=st.turn_id AND ue.thread_id=st.run_id AND ue.session_id=p.session_id AND ue.user=? AND ue.kind='chat'
		WHERE st.run_id=? ORDER BY st.created_at,st.rowid,ue.id`, u.Name, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaborationStepUsage{}
	for rows.Next() {
		var row CollaborationStepUsage
		var id, input, output, read, write, amount sql.NullInt64
		var price, model sql.NullString
		if err = rows.Scan(&row.StepID, &row.RunID, &row.TurnID, &row.Kind, &row.Role, &row.State, &id, &input, &output, &read, &write, &amount, &price, &model); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].StepID != row.StepID {
			row.Status = "unavailable"
			if row.State == "accepted" || row.State == "starting" || row.State == "running" || row.State == "finalizing" {
				row.Status = "pending"
			}
			row.Models = []string{}
			out = append(out, row)
		}
		step := &out[len(out)-1]
		if !id.Valid {
			continue
		}
		if step.InputTokens == nil {
			step.InputTokens, step.OutputTokens, step.CacheReadTokens, step.CacheWriteTokens = new(int64), new(int64), new(int64), new(int64)
			step.TotalTokens, step.CostMicroUSD = new(int64), new(int64)
			step.Status = "partial"
			if step.State == "completed" {
				step.Status = "settled"
			}
		}
		for _, value := range []struct {
			target *int64
			value  sql.NullInt64
		}{{step.InputTokens, input}, {step.OutputTokens, output}, {step.CacheReadTokens, read}, {step.CacheWriteTokens, write}} {
			if !value.value.Valid || value.value.Int64 < 0 || value.value.Int64 > math.MaxInt64-*value.target || value.value.Int64 > math.MaxInt64-*step.TotalTokens {
				return nil, ErrChatInvalid
			}
			*value.target += value.value.Int64
			*step.TotalTokens += value.value.Int64
		}
		var snapshot PriceSnapshot
		if !price.Valid || json.Unmarshal([]byte(price.String), &snapshot) != nil || (snapshot.Source != "table" && snapshot.Source != "provider") {
			step.CostMicroUSD = nil
			step.Status = "partial"
		}
		if step.CostMicroUSD != nil {
			if !amount.Valid || amount.Int64 < 0 || amount.Int64 > math.MaxInt64-*step.CostMicroUSD {
				return nil, ErrChatInvalid
			}
			*step.CostMicroUSD += amount.Int64
		}
		if model.Valid && model.String != "" {
			found := false
			for _, existing := range step.Models {
				found = found || existing == model.String
			}
			if !found {
				step.Models = append(step.Models, model.String)
			}
		}
	}
	return out, rows.Err()
}
