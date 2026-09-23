ALTER TABLE usage_events ADD COLUMN price_snapshot TEXT NOT NULL DEFAULT '' CHECK (price_snapshot = '' OR json_valid(price_snapshot));
