package store

// 额度账本。余额是钱，所以这里的规矩比别处严：
//
//  1. **有 quotas 行才计额度**，没有行等于不限额。老部署升级上来一行都没有，
//     所有人照旧畅通；管理员给谁开额度，谁才开始被计。
//  2. **每笔变动都进 credit_ledger，且带唯一 ref**（幂等键）。同一个 ref 重放
//     不会重复扣钱——网络重试、进程重启后的补账都靠它兜底。
//  3. **扣减与产生它的那件事同事务**：用量行的插入和对应的扣款写在一个事务里
//     （见 InsertUsage），不存在「用量记了但钱没扣」或反过来的中间态。
//
// quotas 里的 balance 是账本的物化缓存，只为免去每次查询都 SUM 全表；两者永远
// 在同一事务里一起改。对不上账时以 credit_ledger 为准重算。

import (
	"database/sql"
	"fmt"
	"time"
)

// 账本条目的 reason 取值。
const (
	ReasonSpend  = "spend"  // 对话消耗，ref 形如 usage:<usage_events.id>
	ReasonGrant  = "grant"  // 管理员充值
	ReasonAdjust = "adjust" // 管理员手工冲正（负数充值）
)

// Quota is one user's credit state. A user without a row here is unmetered:
// spend is still recorded in usage_events, it just isn't charged to anything.
type Quota struct {
	User string `json:"user"`
	// Enforced decides whether an empty balance blocks new turns. False keeps
	// charging but never blocks — useful to watch what a user would spend
	// before actually turning the tap off.
	Enforced bool `json:"enforced"`
	// BalanceMicroUSD is what is left, in millionths of a USD. It can go
	// negative: a turn's cost is only known when the turn ends, so the turn
	// that empties the balance is allowed to overshoot it.
	BalanceMicroUSD int64 `json:"balance_micro_usd"`
	// GrantedMicroUSD/SpentMicroUSD 是两个只增计数器：账本里所有进项之和与所有
	// 出项之和。管理员的负数冲正算进 Spent——它确实是从余额里扣走的钱。要把
	// 「对话花的」和「冲正扣的」分开，查 credit_ledger 的 reason。
	GrantedMicroUSD int64     `json:"granted_micro_usd"`
	SpentMicroUSD   int64     `json:"spent_micro_usd"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// LedgerEntry is one immutable credit movement.
type LedgerEntry struct {
	ID            int64     `json:"id"`
	TS            time.Time `json:"ts"`
	User          string    `json:"user"`
	Ref           string    `json:"ref"`
	Reason        string    `json:"reason"`
	DeltaMicroUSD int64     `json:"delta_micro_usd"`
	BalanceAfter  int64     `json:"balance_after"`
	Note          string    `json:"note,omitempty"`
	Actor         string    `json:"actor,omitempty"` // 谁操作的（充值/冲正时）
}

const quotaCols = `user, enforced, balance_micro_usd, granted_micro_usd, spent_micro_usd,
	created_at, updated_at`

func scanQuota(row interface{ Scan(...any) error }) (Quota, error) {
	var q Quota
	var created, updated string
	if err := row.Scan(&q.User, &q.Enforced, &q.BalanceMicroUSD, &q.GrantedMicroUSD,
		&q.SpentMicroUSD, &created, &updated); err != nil {
		return Quota{}, err
	}
	q.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	q.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return q, nil
}

// GetQuota reports a user's credit state. The bool is false when the user has
// no quota row at all, which means unlimited — not zero balance.
func (s *Store) GetQuota(user string) (Quota, bool) {
	q, err := scanQuota(s.db.QueryRow("SELECT "+quotaCols+" FROM quotas WHERE user = ?", user))
	if err != nil {
		return Quota{}, false
	}
	return q, true
}

// ListQuotas returns every metered user keyed by name, for joining onto a user
// list. Users missing from the map are unmetered.
func (s *Store) ListQuotas() map[string]Quota {
	rows, err := s.db.Query("SELECT " + quotaCols + " FROM quotas")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]Quota{}
	for rows.Next() {
		q, err := scanQuota(rows)
		if err != nil {
			continue
		}
		out[q.User] = q
	}
	return out
}

// quotaExistsTx reports whether the user is metered at all. Checked inside the
// usage transaction so the answer can't change between check and charge.
func quotaExistsTx(tx *sql.Tx, user string) bool {
	var one int
	return tx.QueryRow("SELECT 1 FROM quotas WHERE user = ?", user).Scan(&one) == nil
}

// ensureQuotaTx creates the row for a user who has none yet, at zero balance.
func ensureQuotaTx(tx *sql.Tx, user string) error {
	now := time.Now().Format(time.RFC3339Nano)
	_, err := tx.Exec(`INSERT OR IGNORE INTO quotas
		(user, enforced, balance_micro_usd, granted_micro_usd, spent_micro_usd, created_at, updated_at)
		VALUES (?, 1, 0, 0, 0, ?, ?)`, user, now, now)
	return err
}

// SetQuotaEnforced switches blocking on or off, creating the quota row (at zero
// balance) if the user had none. Turning it on for a user with no credit blocks
// them immediately — grant first, then enforce.
func (s *Store) SetQuotaEnforced(user string, enforced bool) (Quota, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Quota{}, err
	}
	defer tx.Rollback()
	if err := ensureQuotaTx(tx, user); err != nil {
		return Quota{}, err
	}
	if _, err := tx.Exec("UPDATE quotas SET enforced = ?, updated_at = ? WHERE user = ?",
		enforced, time.Now().Format(time.RFC3339Nano), user); err != nil {
		return Quota{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quota{}, err
	}
	q, _ := s.GetQuota(user)
	return q, nil
}

// RemoveQuota puts a user back to unlimited. The balance is dropped (re-adding
// a quota starts from zero); credit_ledger keeps the history either way, so the
// old movements stay auditable.
func (s *Store) RemoveQuota(user string) error {
	_, err := s.db.Exec("DELETE FROM quotas WHERE user = ?", user)
	return err
}

// ErrDuplicateRef reports that a ref was already applied, so nothing changed.
// Callers that retry can treat it as success.
var ErrDuplicateRef = fmt.Errorf("credit ref already applied")

// Grant moves credit for a user, creating the quota row if needed. delta is in
// micro-USD: positive tops up, negative takes back. ref is the idempotency key —
// replaying the same ref returns ErrDuplicateRef and changes nothing, so a
// double-clicked top-up button cannot credit twice.
func (s *Store) Grant(user string, delta int64, ref, note, actor string) (Quota, error) {
	if delta == 0 {
		return Quota{}, fmt.Errorf("充值金额不能为 0")
	}
	if ref == "" {
		return Quota{}, fmt.Errorf("缺少幂等键 ref")
	}
	reason := ReasonGrant
	if delta < 0 {
		reason = ReasonAdjust
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Quota{}, err
	}
	defer tx.Rollback()
	if err := ensureQuotaTx(tx, user); err != nil {
		return Quota{}, err
	}
	applied, err := applyCreditTx(tx, user, delta, ref, reason, note, actor)
	if err != nil {
		return Quota{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quota{}, err
	}
	q, _ := s.GetQuota(user)
	if !applied {
		return q, ErrDuplicateRef
	}
	return q, nil
}

// applyCreditTx is the single place a balance changes: it writes the ledger row
// and moves quotas.balance together, or does neither.
//
// Returns false when ref was already applied (nothing changed). The pre-check
// plus the UNIQUE index on ref make this idempotent two ways over: the check
// handles the ordinary replay, the index is the backstop that fails the whole
// transaction if two writers ever slip past it.
//
// Callers must have created the quota row already (see ensureQuotaTx); a user
// with no row is unmetered and must not reach here.
func applyCreditTx(tx *sql.Tx, user string, delta int64, ref, reason, note, actor string) (bool, error) {
	var dup int
	switch err := tx.QueryRow("SELECT 1 FROM credit_ledger WHERE ref = ?", ref).Scan(&dup); {
	case err == nil:
		return false, nil
	case err != sql.ErrNoRows:
		return false, err
	}

	var balance int64
	if err := tx.QueryRow("SELECT balance_micro_usd FROM quotas WHERE user = ?", user).Scan(&balance); err != nil {
		return false, fmt.Errorf("read balance %s: %w", user, err)
	}
	balance += delta

	// granted/spent 是两个只增计数器，报表用它们区分「充了多少」和「花了多少」，
	// 光看 balance 是看不出来的。
	granted, spent := int64(0), int64(0)
	if delta > 0 {
		granted = delta
	} else {
		spent = -delta
	}
	now := time.Now().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE quotas SET balance_micro_usd = ?,
		granted_micro_usd = granted_micro_usd + ?, spent_micro_usd = spent_micro_usd + ?,
		updated_at = ? WHERE user = ?`, balance, granted, spent, now, user); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO credit_ledger
		(ts, user, ref, reason, delta_micro_usd, balance_after, note, actor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		now, user, ref, reason, delta, balance, note, actor); err != nil {
		return false, fmt.Errorf("ledger %s: %w", ref, err)
	}
	return true, nil
}

// LedgerFilter narrows a ledger query. The zero value matches everything.
type LedgerFilter struct {
	User   string    // empty: every user
	Reason string    // empty: every reason
	Since  time.Time // zero: no lower bound (inclusive)
	Until  time.Time // zero: no upper bound (exclusive)
	Limit  int       // <= 0: no limit
}

// ListLedger returns matching credit movements, newest first.
func (s *Store) ListLedger(f LedgerFilter) []LedgerEntry {
	q := `SELECT id, ts, user, ref, reason, delta_micro_usd, balance_after, note, actor
		FROM credit_ledger WHERE 1=1`
	var args []any
	if f.User != "" {
		q += " AND user = ?"
		args = append(args, f.User)
	}
	if f.Reason != "" {
		q += " AND reason = ?"
		args = append(args, f.Reason)
	}
	if !f.Since.IsZero() {
		q += " AND ts >= ?"
		args = append(args, f.Since.Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		q += " AND ts < ?"
		args = append(args, f.Until.Format(time.RFC3339Nano))
	}
	q += " ORDER BY ts DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.User, &e.Ref, &e.Reason,
			&e.DeltaMicroUSD, &e.BalanceAfter, &e.Note, &e.Actor); err != nil {
			continue
		}
		e.TS, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, e)
	}
	return out
}

// RecomputeBalance re-derives a user's balance from the ledger and reports both
// numbers. The cached balance in quotas should always equal the ledger sum; a
// mismatch means something wrote around applyCreditTx and needs looking at.
//
// Only movements since the quota row was created count: a user whose quota was
// removed and re-added starts from zero again, while their older ledger rows
// stay on file as history.
func (s *Store) RecomputeBalance(user string) (cached, fromLedger int64, err error) {
	q, ok := s.GetQuota(user)
	if !ok {
		return 0, 0, fmt.Errorf("用户 %s 未开额度", user)
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(delta_micro_usd), 0) FROM credit_ledger
		WHERE user = ? AND ts >= ?`, user, q.CreatedAt.Format(time.RFC3339Nano)).
		Scan(&fromLedger); err != nil {
		return 0, 0, err
	}
	return q.BalanceMicroUSD, fromLedger, nil
}
