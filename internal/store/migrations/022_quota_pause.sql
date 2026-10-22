-- Five-hour pause (server/quota_pause.go). pause_percent is the workspace's
-- threshold: 0 follows the administrator's default, -1 turns it off.
ALTER TABLE workspace_keepalive ADD COLUMN pause_percent INTEGER NOT NULL DEFAULT 0;

-- The latest five-hour window a Claude subscription account's responses
-- reported, so a pause outlives a restart. One row per account.
CREATE TABLE account_rate_windows (
    account_id TEXT PRIMARY KEY,
    percent REAL NOT NULL,
    resets_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
