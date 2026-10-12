package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
)

type CollaborationDecisionRecord struct {
	RunID    string             `json:"run_id"`
	State    string             `json:"state"`
	Revision int64              `json:"revision"`
	Snapshot *decision.Snapshot `json:"snapshot,omitempty"`
	Result   *decision.Result   `json:"result,omitempty"`
	Attempt  *DecisionAttempt   `json:"attempt,omitempty"`
}

type DecisionAttempt struct {
	ID               string `json:"id"`
	RunID            string `json:"run_id"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	ActualModel      string `json:"actual_model,omitempty"`
	State            string `json:"state"`
	ReservedMicroUSD int64  `json:"reserved_micro_usd"`
	CostMicroUSD     int64  `json:"cost_micro_usd"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	TokensKnown      bool   `json:"tokens_known"`
	ErrorCode        string `json:"error_code,omitempty"`
	CanReconcile     bool   `json:"can_reconcile"`
	evidenceJSON     string
}

const decisionAttemptCols = "id,run_id,provider,model,actual_model,state,reserved_micro_usd,cost_micro_usd,input_tokens,output_tokens,error_code,evidence_json"

func scanDecisionAttempt(row interface{ Scan(...any) error }) (DecisionAttempt, error) {
	var a DecisionAttempt
	err := row.Scan(&a.ID, &a.RunID, &a.Provider, &a.Model, &a.ActualModel, &a.State, &a.ReservedMicroUSD, &a.CostMicroUSD, &a.InputTokens, &a.OutputTokens, &a.ErrorCode, &a.evidenceJSON)
	var evidence decision.Usage
	if err == nil && json.Unmarshal([]byte(a.evidenceJSON), &evidence) == nil {
		a.TokensKnown = evidence.Known && evidence.InputTokens == a.InputTokens && evidence.OutputTokens == a.OutputTokens
		if a.ErrorCode != "" {
			a.CanReconcile = evidence.CostStatus == "not_dispatched" || (evidence.CostStatus == "known" && evidence.Known)
		}
	}
	return a, err
}

func validateCollaborationDecision(in CollaborationCreateInput, snap decision.Snapshot) error {
	if in.Decision == nil || in.Decision.Validate() != nil || snap.Version != decision.Version || snap.ConfigRevision < 0 || (snap.Provider != "builtin" && snap.Provider != "jev") || len(snap.Roles) != 2 {
		return ErrChatInvalid
	}
	if in.Workflow.IsDirect() && in.Decision.Roles[collaboration.Coordinator].Mode != "fixed" {
		return ErrChatInvalid
	}
	if in.Decision.Provider == "builtin" && snap.Provider != "builtin" {
		return ErrChatInvalid
	}
	if err := validateDecisionSnapshotDigest(&snap); err != nil {
		return err
	}
	for _, role := range []collaboration.Role{collaboration.Coordinator, collaboration.Executor} {
		original, _ := in.Roles.Binding(role)
		r, ok := snap.Roles[role]
		if !ok || r.Mode != in.Decision.Roles[role].Mode {
			return ErrChatInvalid
		}
		if r.Mode == "fixed" {
			if r.Binding != original || len(r.Candidates) != 0 {
				return ErrChatInvalid
			}
			continue
		}
		if r.Fallback != original || len(r.Candidates) == 0 || len(r.Candidates) > 501 {
			return ErrChatInvalid
		}
		seen := map[string]bool{}
		fallback := false
		for _, c := range r.Candidates {
			b := c.Binding
			if !validChatKey(c.Ref) || seen[c.Ref] || len(c.Description) > 4096 || (collaboration.Roles{Coordinator: b, Executor: b}).Validate() != nil || b.AgentType != original.AgentType || b.AccountID != original.AccountID || b.Effort != original.Effort || b.EffortControl != original.EffortControl {
				return ErrChatInvalid
			}
			seen[c.Ref] = true
			fallback = fallback || b == r.Fallback
		}
		if !fallback {
			return ErrChatInvalid
		}
	}
	raw, err := json.Marshal(snap)
	if err != nil || len(raw) > 1<<20 {
		return ErrChatInvalid
	}
	return nil
}

func decisionCandidateDigest(snap *decision.Snapshot) (string, error) {
	raw, err := json.Marshal(snap.Roles)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateDecisionSnapshotDigest(snap *decision.Snapshot) error {
	if snap == nil || snap.Version != decision.Version || snap.TemplateVersion != decision.TemplateVersion {
		return ErrChatInvalid
	}
	digest, err := decisionCandidateDigest(snap)
	if err != nil {
		return err
	}
	if digest != snap.CandidateVersion {
		return ErrChatInvalid
	}
	return nil
}

func collaborationDecisionTx(tx *sql.Tx, run string) (CollaborationDecisionRecord, error) {
	var d CollaborationDecisionRecord
	var snap, result string
	err := tx.QueryRow("SELECT run_id,state,revision,snapshot_json,result_json FROM collaboration_decisions WHERE run_id=?", run).Scan(&d.RunID, &d.State, &d.Revision, &snap, &result)
	if err != nil {
		return d, err
	}
	if json.Unmarshal([]byte(snap), &d.Snapshot) != nil || json.Unmarshal([]byte(result), &d.Result) != nil {
		return d, ErrChatInvalid
	}
	if d.State != "deleted" {
		if err = validateDecisionSnapshotDigest(d.Snapshot); err != nil {
			return d, err
		}
	}
	a, err := scanDecisionAttempt(tx.QueryRow("SELECT "+decisionAttemptCols+" FROM decision_attempts WHERE run_id=?", run))
	if err == nil {
		d.Attempt = &a
	} else if !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	return d, nil
}

func (s *Store) CollaborationDecision(u User, run string) (CollaborationDecisionRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationDecisionRecord{}, err
	}
	defer tx.Rollback()
	if _, err = collaborationRunTx(tx, u, run); err != nil {
		return CollaborationDecisionRecord{}, err
	}
	return collaborationDecisionTx(tx, run)
}

// ClaimCollaborationDecision grants routing once. Reading/replaying a claim
// never grants another provider dispatch, including after restart recovery.
func (s *Store) ClaimCollaborationDecision(u User, run string, revision int64) (CollaborationDecisionRecord, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationDecisionRecord{}, false, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return CollaborationDecisionRecord{}, false, err
	}
	d, err := collaborationDecisionTx(tx, run)
	if err != nil {
		return d, false, err
	}
	if d.State != "pending" {
		return d, false, nil
	}
	if d.Revision != revision || r.State != CollabAccepted || r.ConfigVersion != 1 {
		return d, false, ErrChatConflict
	}
	if _, err = tx.Exec("UPDATE collaboration_decisions SET state='claimed',revision=revision+1,updated_at=? WHERE run_id=? AND revision=?", usageTimestamp(time.Now()), run, revision); err != nil {
		return d, false, err
	}
	d.State = "claimed"
	d.Revision++
	return d, true, tx.Commit()
}

// BeginCollaborationDecisionAttempt reserves task and user credit atomically
// BEFORE networking. The unique run key fences all duplicate dispatch paths.
func (s *Store) BeginCollaborationDecisionAttempt(u User, run string, revision int64, id, provider, model string, maxCost int64) (DecisionAttempt, bool, error) {
	if !collaboration.ValidID(id) || (provider != "jev" && provider != "mindshub") || !config.ValidModelID(model) || maxCost < 0 || maxCost > 1_000_000_000_000 {
		return DecisionAttempt{}, false, ErrChatInvalid
	}
	// A zero reservation is justified only by MindsHub's separately verified
	// promotion, never by omitting a paid provider's price or token evidence.
	if (provider == "jev" && maxCost == 0) || (provider == "mindshub" && (model != config.DecisionJevModel || maxCost != 0)) {
		return DecisionAttempt{}, false, ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionAttempt{}, false, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return DecisionAttempt{}, false, err
	}
	old, err := scanDecisionAttempt(tx.QueryRow("SELECT "+decisionAttemptCols+" FROM decision_attempts WHERE run_id=?", run))
	if err == nil {
		return old, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return old, false, err
	}
	d, err := collaborationDecisionTx(tx, run)
	if err != nil {
		return old, false, err
	}
	if d.State != "claimed" || d.Revision != revision || d.Snapshot == nil || d.Snapshot.Provider != "jev" || r.State != CollabAccepted {
		return old, false, ErrChatConflict
	}
	cost, err := collaborationCostTx(tx, run)
	if err != nil {
		return old, false, err
	}
	if cost.Partial || (r.BudgetMicroUSD > 0 && (cost.CostMicroUSD >= r.BudgetMicroUSD || maxCost > r.BudgetMicroUSD-cost.CostMicroUSD-cost.DecisionReservedMicroUSD)) {
		return old, false, decision.ErrBudgetUnknown
	}
	q, err := scanQuota(tx.QueryRow("SELECT "+quotaCols+" FROM quotas WHERE user=?", u.Name))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return old, false, err
	}
	if err == nil && q.Enforced {
		reserved, e := decisionReservedTx(tx, u.Name, ownerEpoch(u))
		if e != nil {
			return old, false, e
		}
		if q.BalanceMicroUSD < reserved || maxCost > q.BalanceMicroUSD-reserved {
			return old, false, decision.ErrBudgetUnknown
		}
	}
	now := usageTimestamp(time.Now())
	a := DecisionAttempt{ID: id, RunID: run, Provider: provider, Model: model, State: "dispatched", ReservedMicroUSD: maxCost}
	if _, err = tx.Exec("INSERT INTO decision_attempts(id,run_id,provider,model,state,reserved_micro_usd,created_at,updated_at) VALUES(?,?,?,?,'dispatched',?,?,?)", id, run, provider, model, maxCost, now, now); err != nil {
		return a, false, err
	}
	return a, true, tx.Commit()
}

// SettleCollaborationDecisionAttempt may accept late fee evidence after the
// model binding was frozen/cancelled/deleted. It never grants execution rights.
func validateDecisionUsage(usage decision.Usage, actualModel string) error {
	if usage.InputTokens < 0 || usage.InputTokens > 1_000_000_000_000 || usage.OutputTokens < 0 || usage.OutputTokens > 1_000_000_000_000 || usage.CostMicroUSD < 0 || usage.CostMicroUSD > 1_000_000_000_000 || len(actualModel) > 256 {
		return ErrChatInvalid
	}
	if usage.CostStatus != "known" && usage.CostStatus != "unknown" && usage.CostStatus != "not_dispatched" {
		return ErrChatInvalid
	}
	if usage.CostStatus == "not_dispatched" && (usage.CostMicroUSD != 0 || usage.InputTokens != 0 || usage.OutputTokens != 0) {
		return ErrChatInvalid
	}
	if usage.CostStatus == "known" && !usage.Known {
		return ErrChatInvalid
	}
	return nil
}

func (s *Store) SettleCollaborationDecisionAttempt(u User, run, id string, usage decision.Usage, actualModel string) error {
	if err := validateDecisionUsage(usage, actualModel); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = settleDecisionAttemptTx(tx, u, run, id, usage, actualModel); err != nil {
		return err
	}
	return tx.Commit()
}

func mergeDecisionEvidence(a DecisionAttempt, usage decision.Usage, actualModel string) (decision.Usage, string, error) {
	var prior decision.Usage
	if a.evidenceJSON != "" && a.evidenceJSON != "null" {
		if err := json.Unmarshal([]byte(a.evidenceJSON), &prior); err != nil {
			return usage, actualModel, err
		}
	}
	if prior.Known {
		if usage.CostStatus == "not_dispatched" {
			return usage, actualModel, ErrChatConflict
		}
		if usage.Known && (prior.InputTokens != usage.InputTokens || prior.OutputTokens != usage.OutputTokens) {
			return usage, actualModel, ErrChatConflict
		}
		if prior.CostStatus == "known" && usage.CostStatus == "known" && prior.CostMicroUSD != usage.CostMicroUSD {
			return usage, actualModel, ErrChatConflict
		}
		if actualModel != "" && a.ActualModel != "" && actualModel != a.ActualModel {
			return usage, actualModel, ErrChatConflict
		}
		if !usage.Known {
			usage = prior
		}
	}
	if prior.CostStatus == "not_dispatched" {
		if usage.CostStatus == "known" || usage.Known {
			return usage, actualModel, ErrChatConflict
		}
		usage = prior
	}
	if actualModel == "" {
		actualModel = a.ActualModel
	}
	return usage, actualModel, nil
}

func settleDecisionAttemptTx(tx *sql.Tx, u User, run, id string, usage decision.Usage, actualModel string) error {
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	a, err := scanDecisionAttempt(tx.QueryRow("SELECT "+decisionAttemptCols+" FROM decision_attempts WHERE run_id=? AND id=?", run, id))
	if err != nil {
		return err
	}
	if a.State == "settled" || a.State == "not_dispatched" {
		if usage.CostStatus == "unknown" {
			return nil
		}
		if (a.State == "settled") != (usage.CostStatus == "known") || a.CostMicroUSD != usage.CostMicroUSD || a.InputTokens != usage.InputTokens || a.OutputTokens != usage.OutputTokens || a.ActualModel != actualModel {
			return ErrChatConflict
		}
		return nil
	}
	usage, actualModel, err = mergeDecisionEvidence(a, usage, actualModel)
	if err != nil {
		return err
	}
	state, reserved := "unknown", a.ReservedMicroUSD
	if usage.CostStatus == "known" {
		state, reserved = "settled", 0
	}
	if usage.CostStatus == "not_dispatched" {
		state, reserved = "not_dispatched", 0
	}
	if state == "settled" {
		model := actualModel
		if model == "" {
			model = a.Model
		}
		price := &PriceSnapshot{Version: 1, Source: "table", Key: model, Standard: TokenRates{Input: decision.JevInputUSDPerMillion}, PricingRevision: decision.JevPricingVersion, CatalogVersion: decision.JevPricingVersion, SourceURL: decision.JevPricingSource, VerifiedAt: decision.JevPricingVerifiedAt}
		if a.Provider == "mindshub" {
			if model != config.DecisionJevModel || usage.CostMicroUSD != 0 || usage.InputTokens > decision.MindshubJevMaxInputTokens {
				return ErrChatInvalid
			}
			price = &PriceSnapshot{Version: 1, Source: "table", Key: model, Standard: TokenRates{}, PricingRevision: decision.MindshubJevPricingVersion, CatalogVersion: decision.MindshubJevPricingVersion, SourceURL: decision.MindshubJevPricingSource, VerifiedAt: decision.MindshubJevPricingVerifiedAt}
		}
		ev := UsageEvent{TS: time.Now(), User: u.Name, SessionID: r.SourceSessionID, ThreadID: run, TurnID: id, Agent: "decision", Model: model, Kind: UsageKindDecision, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CostMicroUSD: usage.CostMicroUSD, Provider: a.Provider, ReqID: "decision:" + id, Price: price}
		if err = insertUsageTx(tx, []UsageEvent{ev}); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(usage)
	errorCode := a.ErrorCode
	if state != "unknown" {
		errorCode = ""
	}
	_, err = tx.Exec("UPDATE decision_attempts SET state=?,reserved_micro_usd=?,cost_micro_usd=?,input_tokens=?,output_tokens=?,actual_model=?,evidence_json=?,error_code=?,updated_at=? WHERE id=?", state, reserved, usage.CostMicroUSD, usage.InputTokens, usage.OutputTokens, actualModel, string(raw), errorCode, usageTimestamp(time.Now()), id)
	if err != nil {
		return err
	}
	// Financial evidence has its own persistence. It must not invalidate a
	// preparation/cancellation owner's run revision or a routing claim.
	if r.State == CollabPaused && r.WaitReason == "decision_persistence_failed" && state != "unknown" {
		r.WaitReason = "decision_failed"
		if err = saveCollaborationTx(tx, &r); err != nil {
			return err
		}
	}
	return nil
}

// FailCollaborationDecisionSettlement retains evidence when the atomic usage /
// ledger transaction failed. No provider retry is necessary to reconcile it.
func (s *Store) FailCollaborationDecisionSettlement(u User, run, id string, usage decision.Usage, actualModel string) error {
	if err := validateDecisionUsage(usage, actualModel); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	a, err := scanDecisionAttempt(tx.QueryRow("SELECT "+decisionAttemptCols+" FROM decision_attempts WHERE run_id=? AND id=?", run, id))
	if err != nil {
		return err
	}
	if a.State == "settled" || a.State == "not_dispatched" {
		return nil
	}
	usage, actualModel, err = mergeDecisionEvidence(a, usage, actualModel)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(usage)
	if _, err = tx.Exec("UPDATE decision_attempts SET state='unknown',evidence_json=?,input_tokens=?,output_tokens=?,actual_model=?,error_code='decision_persistence_failed',updated_at=? WHERE id=?", string(raw), usage.InputTokens, usage.OutputTokens, actualModel, usageTimestamp(time.Now()), id); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collaboration_decisions SET state='interrupted',revision=revision+1,updated_at=? WHERE run_id=? AND state IN ('pending','claimed')", usageTimestamp(time.Now()), run); err != nil {
		return err
	}
	switch r.State {
	case CollabDeleted, CollabCancelled, CollabCancelling, CollabFailed, CollabCompleted:
	default:
		var active int
		if err = tx.QueryRow("SELECT COUNT(*) FROM collaboration_steps WHERE run_id=? AND state IN ('starting','running','finalizing','uncertain')", run).Scan(&active); err != nil {
			return err
		}
		if active > 0 || r.State == CollabPreparing {
			if _, err = tx.Exec("UPDATE collaborations SET pause_requested=1 WHERE id=?", run); err != nil {
				return err
			}
			r.WaitReason = "decision_persistence_failed"
		} else {
			pauseCollaboration(&r, "decision_persistence_failed")
		}
		if err = saveCollaborationTx(tx, &r); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Reconcile uses only server-stored fee evidence and checks the user's viewed
// run revision. It settles once; it never invokes a decision provider or CLI.
func (s *Store) ReconcileCollaborationDecisionAttempt(u User, run string, revision int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	if r.Revision != revision {
		return ErrChatConflict
	}
	a, err := scanDecisionAttempt(tx.QueryRow("SELECT "+decisionAttemptCols+" FROM decision_attempts WHERE run_id=?", run))
	if err != nil {
		return err
	}
	if a.ErrorCode == "" {
		return collaboration.ErrState
	}
	var usage decision.Usage
	if json.Unmarshal([]byte(a.evidenceJSON), &usage) != nil || (usage.CostStatus != "known" && usage.CostStatus != "not_dispatched") || validateDecisionUsage(usage, a.ActualModel) != nil {
		return collaboration.ErrState
	}
	if err = settleDecisionAttemptTx(tx, u, run, a.ID, usage, a.ActualModel); err != nil {
		return err
	}
	return tx.Commit()
}

func validateDecisionResult(snap *decision.Snapshot, result decision.Result) error {
	if snap == nil || result.Roles.Validate() != nil || len(result.Decisions) != 2 || len(result.ProviderModel) > 256 {
		return ErrChatInvalid
	}
	for _, role := range []collaboration.Role{collaboration.Coordinator, collaboration.Executor} {
		r, ok := snap.Roles[role]
		if !ok {
			return ErrChatInvalid
		}
		binding, _ := result.Roles.Binding(role)
		d, ok := result.Decisions[role]
		if !ok || d.Binding != binding || len(d.Reason) > 1024 {
			return ErrChatInvalid
		}
		if r.Mode == "fixed" {
			if binding != r.Binding || d.Source != "manual" {
				return ErrChatInvalid
			}
			continue
		}
		if d.Source != "builtin" && d.Source != "jev" && d.Source != "fallback" {
			return ErrChatInvalid
		}
		found := false
		for _, c := range r.Candidates {
			found = found || c.Binding == binding
		}
		if !found {
			return ErrChatInvalid
		}
		if d.Source == "fallback" && binding != r.Fallback {
			return ErrChatInvalid
		}
	}
	return nil
}

func (s *Store) FreezeCollaborationDecision(u User, run string, revision int64, result decision.Result) (CollaborationRun, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return CollaborationRun{}, err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return r, err
	}
	d, err := collaborationDecisionTx(tx, run)
	if err != nil {
		return r, err
	}
	if err = validateDecisionResult(d.Snapshot, result); err != nil {
		return r, err
	}
	pausedRouting := r.State == CollabPaused && r.ResumeState == CollabAccepted && r.WaitReason == "user_paused"
	if (r.State != CollabAccepted && !pausedRouting) || r.ConfigVersion != 1 || d.Revision != revision || (d.State != "claimed" && d.State != "interrupted") {
		return r, ErrChatConflict
	}
	if d.Attempt != nil && d.Attempt.State == "dispatched" {
		return r, collaboration.ErrState
	}
	for _, v := range result.Decisions {
		if v.Source == "jev" && (d.Snapshot.Provider != "jev" || d.Attempt == nil || d.Attempt.State == "not_dispatched") {
			return r, ErrChatInvalid
		}
	}
	// Restart recovery permits only an explicit resumed fallback, never a
	// delayed pre-restart recommendation or another paid classification attempt.
	if d.State == "interrupted" {
		for _, v := range result.Decisions {
			if v.Source != "manual" && v.Source != "fallback" {
				return r, ErrChatConflict
			}
		}
	}
	var raw string
	var cfg CollaborationConfig
	if err = tx.QueryRow("SELECT config_json FROM collaboration_configs WHERE run_id=? AND version=1", run).Scan(&raw); err != nil {
		return r, err
	}
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return r, err
	}
	cfg.Roles = result.Roles
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return r, err
	}
	if _, err = tx.Exec("UPDATE collaboration_configs SET config_json=? WHERE run_id=? AND version=1", string(encoded), run); err != nil {
		return r, err
	}
	encoded, err = json.Marshal(result)
	if err != nil {
		return r, err
	}
	if _, err = tx.Exec("UPDATE collaboration_decisions SET state='frozen',result_json=?,revision=revision+1,updated_at=? WHERE run_id=? AND revision=?", string(encoded), usageTimestamp(time.Now()), run, revision); err != nil {
		return r, err
	}
	if err = collaborationEventTx(tx, run, "decision", result); err != nil {
		return r, err
	}
	if err = saveCollaborationTx(tx, &r); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// FailCollaborationDecision leaves a stopped, explicitly resumable task. Its
// interrupted decision can only use frozen fallbacks after that user action.
func (s *Store) FailCollaborationDecision(u User, run string, revision int64, reason string) error {
	if reason != "decision_failed" && reason != "authorization_revoked" && reason != "decision_persistence_failed" {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := collaborationRunTx(tx, u, run)
	if err != nil {
		return err
	}
	d, err := collaborationDecisionTx(tx, run)
	if err != nil {
		return err
	}
	pausedRouting := r.State == CollabPaused && r.ResumeState == CollabAccepted && r.WaitReason == "user_paused"
	if (r.State != CollabAccepted && !pausedRouting) || d.Revision != revision || d.State == "frozen" || d.State == "deleted" {
		return ErrChatConflict
	}
	if _, err = tx.Exec("UPDATE collaboration_decisions SET state='interrupted',revision=revision+1,updated_at=? WHERE run_id=?", usageTimestamp(time.Now()), run); err != nil {
		return err
	}
	pauseCollaboration(&r, reason)
	if err = saveCollaborationTx(tx, &r); err != nil {
		return err
	}
	return tx.Commit()
}

func collaborationDecisionReadyTx(tx *sql.Tx, run string) error {
	var state string
	err := tx.QueryRow("SELECT state FROM collaboration_decisions WHERE run_id=?", run).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		var raw string
		if err = tx.QueryRow("SELECT request_json FROM collaborations WHERE id=?", run).Scan(&raw); err != nil {
			return err
		}
		var in CollaborationCreateInput
		if err = json.Unmarshal([]byte(raw), &in); err != nil {
			return err
		}
		if in.Decision != nil {
			return collaboration.ErrState
		}
		return nil
	}
	if err != nil {
		return err
	}
	if state != "frozen" {
		return collaboration.ErrState
	}
	return nil
}

func decisionReservedTx(tx *sql.Tx, user, epoch string) (int64, error) {
	var amount int64
	err := tx.QueryRow("SELECT COALESCE(SUM(a.reserved_micro_usd),0) FROM decision_attempts a JOIN collaborations c ON c.id=a.run_id WHERE c.user=? AND c.user_created_at=?", user, epoch).Scan(&amount)
	return amount, err
}

func (s *Store) DecisionReservedMicroUSD(user string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var epoch string
	if err = tx.QueryRow("SELECT created_at FROM users WHERE name=?", user).Scan(&epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// This is an accounting query, not an authorization check. Without
			// a current owner identity no reservation belongs to this name;
			// ordinary legacy/unmetered quota semantics remain unchanged.
			return 0, nil
		}
		return 0, err
	}
	return decisionReservedTx(tx, user, epoch)
}

func addDecisionCostTx(tx *sql.Tx, run string, cost *CollaborationCost) error {
	var settled, reserved int64
	var unknown, calls int
	err := tx.QueryRow("SELECT COALESCE(SUM(CASE WHEN state='settled' THEN cost_micro_usd ELSE 0 END),0),COALESCE(SUM(reserved_micro_usd),0),COALESCE(SUM(CASE WHEN state IN ('dispatched','unknown') THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN state!='not_dispatched' THEN 1 ELSE 0 END),0) FROM decision_attempts WHERE run_id=?", run).Scan(&settled, &reserved, &unknown, &calls)
	if err != nil {
		return err
	}
	if settled > math.MaxInt64-cost.CostMicroUSD {
		return ErrChatInvalid
	}
	cost.CostMicroUSD += settled
	cost.DecisionCostMicroUSD = settled
	cost.DecisionReservedMicroUSD = reserved
	cost.DecisionPartial = unknown > 0
	cost.DecisionCalls = calls
	return nil
}

func (s *Store) GetCollaborationDecisionDefault(u User, session string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return "", err
	}
	if err = checkChatSession(tx, u, session); err != nil {
		return "", err
	}
	var provider string
	err = tx.QueryRow("SELECT provider FROM collaboration_decision_defaults WHERE session_id=?", session).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return "inherit", nil
	}
	return provider, err
}

func (s *Store) SetCollaborationDecisionDefault(u User, session, provider string) error {
	if provider != "inherit" && provider != "builtin" && provider != "jev" {
		return ErrChatInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCreationOwner(tx, u); err != nil {
		return err
	}
	if err = checkChatSession(tx, u, session); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO collaboration_decision_defaults(session_id,provider) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET provider=excluded.provider", session, provider); err != nil {
		return err
	}
	return tx.Commit()
}
