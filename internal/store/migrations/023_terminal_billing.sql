-- Terminal and background rows start debiting the balance (store/terminal_billing.go).
-- settled_micro_usd is how much of a row's cost the ledger has already
-- accounted for: a terminal row is upserted again as its request keeps
-- streaming, and only the growth since the last settlement is charged.
-- Rows already on record count as settled, so the upgrade's rescan of old
-- transcripts charges nothing retroactively.
ALTER TABLE usage_events ADD COLUMN settled_micro_usd INTEGER NOT NULL DEFAULT 0;
UPDATE usage_events SET settled_micro_usd = cost_micro_usd WHERE kind IN ('terminal', 'background');

-- When terminal billing began. A request older than this that the scanner only
-- now finds (a transcript it never read before) is recorded but not charged.
CREATE TABLE terminal_billing (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    since TEXT NOT NULL
);
