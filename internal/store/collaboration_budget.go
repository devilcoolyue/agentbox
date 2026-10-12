package store

import (
	"database/sql"
	"encoding/json"
	"math"

	"agentbox/internal/collaboration"
)

type CollaborationCost struct {
	CostMicroUSD             int64 `json:"cost_micro_usd"`
	DecisionCostMicroUSD     int64 `json:"decision_cost_micro_usd"`
	DecisionReservedMicroUSD int64 `json:"decision_reserved_micro_usd"`
	DecisionPartial          bool  `json:"decision_partial"`
	DecisionCalls            int   `json:"decision_calls"`
	Partial                  bool  `json:"partial"`
	Turns                    int   `json:"turns"`
}

func collaborationCostTx(tx *sql.Tx, run string) (CollaborationCost, error) {
	var cost CollaborationCost
	rows, err := tx.Query(`SELECT st.turn_id,ue.cost_micro_usd,ue.price_snapshot FROM collaboration_steps st LEFT JOIN usage_events ue ON ue.turn_id=st.turn_id AND ue.thread_id=st.run_id AND ue.kind='chat' WHERE st.run_id=? AND st.state IN ('completed','reviewed','interrupted','uncertain','running','finalizing')`, run)
	if err != nil {
		return cost, err
	}
	defer rows.Close()
	turns := map[string]bool{}
	for rows.Next() {
		var id string
		var amount sql.NullInt64
		var raw sql.NullString
		if err = rows.Scan(&id, &amount, &raw); err != nil {
			return cost, err
		}
		turns[id] = true
		if !amount.Valid {
			cost.Partial = true
			continue
		}
		if amount.Int64 < 0 || amount.Int64 > math.MaxInt64-cost.CostMicroUSD {
			return cost, collaboration.ErrInvalid
		}
		cost.CostMicroUSD += amount.Int64
		var price PriceSnapshot
		if !raw.Valid || json.Unmarshal([]byte(raw.String), &price) != nil || (price.Source != "table" && price.Source != "provider") {
			cost.Partial = true
		}
	}
	cost.Turns = len(turns)
	if err = rows.Err(); err != nil {
		return cost, err
	}
	if err = rows.Close(); err != nil {
		return cost, err
	}
	err = addDecisionCostTx(tx, run, &cost)
	return cost, err
}
func (s *Store) CollaborationCost(u User, run string) (CollaborationCost, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationCost{}, err
	}
	defer tx.Rollback()
	if _, err = collaborationRunTx(tx, u, run); err != nil {
		return CollaborationCost{}, err
	}
	return collaborationCostTx(tx, run)
}

func pauseForCollaborationBudgetTx(tx *sql.Tx, r *CollaborationRun) (bool, error) {
	var failed int
	if err := tx.QueryRow("SELECT COUNT(*) FROM decision_attempts WHERE run_id=? AND error_code!=''", r.ID).Scan(&failed); err != nil {
		return false, err
	}
	if failed > 0 {
		pauseCollaboration(r, "decision_persistence_failed")
		return true, nil
	}
	if r.BudgetMicroUSD == 0 {
		return false, nil
	}
	cost, err := collaborationCostTx(tx, r.ID)
	if err != nil {
		return false, err
	}
	if cost.Partial {
		pauseCollaboration(r, "usage_incomplete")
		return true, nil
	}
	if cost.CostMicroUSD >= r.BudgetMicroUSD || cost.DecisionReservedMicroUSD >= r.BudgetMicroUSD-cost.CostMicroUSD {
		pauseCollaboration(r, "budget_limit")
		return true, nil
	}
	return false, nil
}
