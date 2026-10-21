-- Per-workspace prompt cache keepalive (server/keepalive.go), set by the
-- workspace's owner. A workspace without a row keeps nothing; zero limits
-- follow the administrator's defaults. The row leaves with the workspace.
CREATE TABLE workspace_keepalive (
    session_id TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL DEFAULT 0,
    terminal INTEGER NOT NULL DEFAULT 0,
    max_hours INTEGER NOT NULL DEFAULT 0,
    min_context_tokens INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
