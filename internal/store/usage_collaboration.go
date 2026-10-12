package store

import "strings"

// UsageCollaboration is read-only attribution of an existing accounting row.
// Decision requests have a run and source session, but are not model steps.
type UsageCollaboration struct {
	RunID           string `json:"run_id"`
	StepID          string `json:"step_id,omitempty"`
	Role            string `json:"role,omitempty"`
	StepKind        string `json:"step_kind,omitempty"`
	SourceSessionID string `json:"source_session_id"`
}

// Keep the participant generation frozen by the original step. A later role
// replacement or task deletion must not move or erase historical attribution.
// Do not join sessions or read request_json: their contents may already be gone.
const usageCollaborationStepTables = `collaboration_steps uc_step
	JOIN collaboration_participants uc_participant ON uc_participant.run_id=uc_step.run_id
		AND uc_participant.role=uc_step.role AND uc_participant.generation=uc_step.participant_generation
	JOIN collaborations uc_run ON uc_run.id=uc_step.run_id`

const usageCollaborationStepMatch = `usage_events.kind='chat'
	AND uc_step.turn_id!='' AND usage_events.turn_id=uc_step.turn_id
	AND usage_events.thread_id=uc_run.id AND usage_events.session_id=uc_participant.session_id
	AND usage_events.user=uc_run.user`

// UsageCollaborations attributes only the requested event IDs. Callers must
// first apply their usage visibility boundary; this method does not authorize
// a user or expose task input. Reads never rewrite usage or debit credit.
func (s *Store) UsageCollaborations(eventIDs []int64) (map[int64]UsageCollaboration, error) {
	out := make(map[int64]UsageCollaboration)
	if len(eventIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Stay below SQLite's variable limit even for unpaged callers. Each batch
	// reads both kinds from the same snapshot without loading event bodies.
	const batchSize = 400
	for start := 0; start < len(eventIDs); start += batchSize {
		end := min(start+batchSize, len(eventIDs))
		args := make([]any, end-start)
		for i, id := range eventIDs[start:end] {
			args[i] = id
		}
		ids := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		queries := []string{
			`SELECT usage_events.id,uc_run.id,uc_step.id,uc_step.role,uc_step.kind,uc_run.source_session_id
			FROM usage_events, ` + usageCollaborationStepTables + ` WHERE ` + usageCollaborationStepMatch +
				` AND usage_events.id IN (` + ids + `)`,
			`SELECT usage_events.id,uc_run.id,'','','',uc_run.source_session_id
			FROM usage_events JOIN decision_attempts uc_attempt ON uc_attempt.id=usage_events.turn_id
			JOIN collaborations uc_run ON uc_run.id=uc_attempt.run_id
			WHERE usage_events.kind='decision' AND uc_attempt.id!=''
				AND usage_events.req_id='decision:' || uc_attempt.id
				AND usage_events.thread_id=uc_run.id AND usage_events.session_id=uc_run.source_session_id
				AND usage_events.user=uc_run.user AND usage_events.id IN (` + ids + `)`,
		}
		for _, query := range queries {
			rows, err := tx.Query(query, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id int64
				var attribution UsageCollaboration
				if err = rows.Scan(&id, &attribution.RunID, &attribution.StepID, &attribution.Role, &attribution.StepKind, &attribution.SourceSessionID); err != nil {
					rows.Close()
					return nil, err
				}
				out[id] = attribution
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
