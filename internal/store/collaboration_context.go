package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"agentbox/internal/collaboration"
)

var errCollaborationContextLimit = errors.New("collaboration required context exceeds input budget")

// This boundary is part of the fingerprinted step input. It describes data
// actually offered to this exact native thread, not merely queued messages.
type CollaborationContextBoundary struct {
	Version         int    `json:"version"`
	ProviderSession string `json:"provider_session,omitempty"`
	After           int64  `json:"after"`
	Through         int64  `json:"through"`
	Rebuilt         bool   `json:"rebuilt"`
	Omitted         int    `json:"omitted"`
}

func collaborationContextTx(tx *sql.Tx, r *CollaborationRun, role string, generation int, budget int) ([]collaboration.ContextMessage, *CollaborationContextBoundary, error) {
	var provider, ackProvider string
	var after int64
	if err := tx.QueryRow("SELECT provider_session,context_provider,context_seq FROM collaboration_participants WHERE run_id=? AND role=? AND generation=?", r.ID, role, generation).Scan(&provider, &ackProvider, &after); err != nil {
		return nil, nil, err
	}
	rebuilt := provider == "" || provider != ackProvider
	if rebuilt {
		after = 0
	}
	boundary := &CollaborationContextBoundary{Version: 1, ProviderSession: provider, After: after, Rebuilt: rebuilt}
	if err := tx.QueryRow("SELECT COALESCE(MAX(seq),0) FROM collaboration_messages WHERE run_id=?", r.ID).Scan(&boundary.Through); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(`SELECT m.seq,m.id,m.sender_role,m.type,m.body_json,
 COALESCE(q.state,''),COALESCE(s.participant_generation,0),COALESCE(s.state,'')
 FROM collaboration_messages m LEFT JOIN collaboration_questions q ON q.run_id=m.run_id AND q.message_id=m.id
 LEFT JOIN collaboration_steps s ON s.id=m.step_id AND s.run_id=m.run_id
 WHERE m.run_id=? AND m.seq>? AND m.seq<=? ORDER BY m.seq`, r.ID, after, boundary.Through)
	if err != nil {
		return nil, nil, err
	}
	type item struct {
		message   collaboration.ContextMessage
		mandatory bool
		cost      int
	}
	var items []item
	latest := map[string]int{}
	used := 0
	for rows.Next() {
		var seq int64
		var raw, qstate, state string
		var gen int
		var m collaboration.ContextMessage
		if err = rows.Scan(&seq, &m.ID, &m.Sender, &m.Type, &raw, &qstate, &gen, &state); err != nil {
			rows.Close()
			return nil, nil, err
		}
		// Its own successfully recorded final answer already exists in the native
		// thread. Do not echo it; different generations still need that evidence.
		if !rebuilt && m.Sender == role && gen == generation && state == "completed" {
			continue
		}
		m.Body = json.RawMessage(raw)
		required := m.Sender == "user" || m.Type == "answer" || qstate == "open"
		if m.Type == "plan" || m.Type == "result" || m.Type == "review" {
			latest[m.Type] = len(items)
		}
		// A superseded plan may contain hard constraints. Preserve them verbatim
		// even when old plan detail is omitted from a reconstructed thread.
		if m.Type == "plan" {
			var plan collaboration.Plan
			if err = json.Unmarshal(m.Body, &plan); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if len(plan.Constraints) > 0 {
				required = true
			}
			for _, location := range plan.CodeMap {
				if len(location.Constraints) > 0 {
					required = true
				}
			}
		}
		// RawMessage may still contain literal HTML-sensitive characters from
		// provider output or an HTTP answer. The final prompt uses json.Marshal,
		// which escapes those characters; measure those actual bytes here too.
		encoded, marshalErr := json.Marshal(m)
		if marshalErr != nil {
			rows.Close()
			return nil, nil, marshalErr
		}
		cost := len(encoded) + 1 // comma in the messages array
		if required {
			used += cost
			if used > budget {
				rows.Close()
				return nil, nil, errCollaborationContextLimit
			}
		}
		items = append(items, item{m, required, cost})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	for _, idx := range latest {
		if !items[idx].mandatory {
			items[idx].mandatory = true
			used += items[idx].cost
		}
	}
	// Reserve the omission marker instead of cutting a JSON object or any user
	// instruction. A required set that cannot fit pauses explicitly.
	if used+4096 > budget {
		return nil, nil, errCollaborationContextLimit
	}
	selected := make([]bool, len(items))
	for i, it := range items {
		selected[i] = it.mandatory
	}
	for i := len(items) - 1; i >= 0; i-- {
		if !selected[i] && used+items[i].cost+4096 <= budget {
			selected[i] = true
			used += items[i].cost
		}
	}
	out := []collaboration.ContextMessage{}
	omittedIDs := []string{}
	for i, it := range items {
		if selected[i] {
			out = append(out, it.message)
		} else {
			boundary.Omitted++
			if len(omittedIDs) < 32 {
				omittedIDs = append(omittedIDs, it.message.ID)
			}
		}
	}
	marker, _ := json.Marshal(map[string]any{"mode": map[bool]string{true: "rebuilt", false: "incremental"}[rebuilt], "after": after, "through": boundary.Through, "omitted_messages": boundary.Omitted, "omitted_ids_sample": omittedIDs, "instruction": "Older optional handoff detail may be omitted. User instructions, answers, unresolved questions, plan constraints and the latest relevant result/review are retained. Omitted content is not proof of completion. Read the current workspace or ask a concrete question if evidence is missing."})
	out = append([]collaboration.ContextMessage{{Sender: "system", Type: "context_boundary", Body: marker}}, out...)
	return out, boundary, nil
}

func acknowledgeCollaborationContextTx(tx *sql.Tx, r *CollaborationRun, st CollaborationStep) error {
	if r.Request == nil || r.Request.Workflow == nil {
		return nil
	}
	var raw, provider string
	if err := tx.QueryRow("SELECT input_json FROM collaboration_steps WHERE id=?", st.ID).Scan(&raw); err != nil {
		return err
	}
	var input CollaborationStepInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return err
	}
	if input.Context == nil {
		return nil
	}
	if err := tx.QueryRow("SELECT provider_session FROM collaboration_participants WHERE run_id=? AND role=? AND generation=?", r.ID, st.Role, st.ParticipantGeneration).Scan(&provider); err != nil {
		return err
	}
	if provider == "" {
		return nil
	}
	if input.Context.ProviderSession != "" && input.Context.ProviderSession != provider {
		return ErrChatConflict
	}
	_, err := tx.Exec("UPDATE collaboration_participants SET context_seq=MAX(context_seq,?),context_provider=? WHERE run_id=? AND role=? AND generation=? AND provider_session=?", input.Context.Through, provider, r.ID, st.Role, st.ParticipantGeneration, provider)
	return err
}
