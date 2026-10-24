package store

import (
	"database/sql"
	"fmt"
)

// Terminal and background rows are charged when they are written, like chat,
// but they arrive differently: the terminal scanner upserts the same request
// again while it is still streaming, so a row's cost can grow after it was
// first charged. usage_events.settled_micro_usd remembers how much of the cost
// the ledger has already accounted for, and each write charges only the growth.
//
// "Settled" is not "charged": growth that is not billable — the user has no
// quota row, or the request predates terminal billing (schema 23) — is settled
// without a ledger entry, so it can never be charged later, e.g. when an
// administrator opens a quota for the user and the next rescan rewrites every
// row. A cost that shrinks (it should not: the parser keeps the larger output)
// is not refunded; the settled mark stays at its high-water value.

// settleUsageTx settles row id of user at cost. ts is the row's stored
// timestamp; billing reads metered users once per transaction through metered.
func settleUsageTx(tx *sql.Tx, id int64, user, ts, note string, cost int64, metered map[string]bool) error {
	var settled int64
	if err := tx.QueryRow("SELECT settled_micro_usd FROM usage_events WHERE id = ?", id).Scan(&settled); err != nil {
		return fmt.Errorf("read settlement %d: %w", id, err)
	}
	delta := cost - settled
	if delta <= 0 {
		return nil
	}
	billable, err := terminalBillableTx(tx, user, ts, metered)
	if err != nil {
		return err
	}
	if billable {
		ref, err := settleRefTx(tx, id)
		if err != nil {
			return err
		}
		if _, err := applyCreditTx(tx, user, -delta, ref, ReasonSpend, note, ""); err != nil {
			return fmt.Errorf("charge usage %d: %w", id, err)
		}
	}
	_, err = tx.Exec("UPDATE usage_events SET settled_micro_usd = ? WHERE id = ?", cost, id)
	return err
}

func terminalBillableTx(tx *sql.Tx, user, ts string, metered map[string]bool) (bool, error) {
	var since string
	switch err := tx.QueryRow("SELECT since FROM terminal_billing WHERE id = 1").Scan(&since); {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	}
	if ts < since {
		return false, nil
	}
	ok, seen := metered[user]
	if !seen {
		ok = quotaExistsTx(tx, user)
		metered[user] = ok
	}
	return ok, nil
}

// settleRefTx is the ledger key of the next charge against row id: usage:<id>
// for the first, as for chat rows, then usage:<id>:2, usage:<id>:3 … for the
// growth after it. Counting what is already there keeps a rescan that charges
// nothing from consuming a key, and the range keeps usage:1 from matching
// usage:12 while still using the unique index on ref.
func settleRefTx(tx *sql.Tx, id int64) (string, error) {
	base := fmt.Sprintf("usage:%d", id)
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM credit_ledger WHERE ref = ? OR (ref >= ? AND ref < ?)",
		base, base+":", base+";").Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return base, nil
	}
	return fmt.Sprintf("%s:%d", base, n+1), nil
}

func settleNote(kind string, e UsageEvent) string {
	label := "终端"
	if kind == UsageKindBackground {
		label = "后台请求"
	}
	if e.Model != "" {
		return label + " · " + e.Model
	}
	if e.Agent != "" {
		return label + " · " + e.Agent
	}
	return label
}
