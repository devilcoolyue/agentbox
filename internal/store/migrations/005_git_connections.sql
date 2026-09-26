CREATE TABLE IF NOT EXISTS git_connections (
 id TEXT PRIMARY KEY,
 owner TEXT NOT NULL,
 label TEXT NOT NULL,
 provider TEXT NOT NULL,
 base_url TEXT NOT NULL,
 auth_type TEXT NOT NULL,
 username TEXT NOT NULL DEFAULT '',
 secret BLOB NOT NULL,
 read_only INTEGER NOT NULL DEFAULT 1,
 enabled INTEGER NOT NULL DEFAULT 1,
 revision INTEGER NOT NULL DEFAULT 1,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_git_connections_owner ON git_connections(owner);
CREATE TABLE IF NOT EXISTS git_bindings (
 session_id TEXT NOT NULL,
 repo TEXT NOT NULL,
 remote TEXT NOT NULL,
 url TEXT NOT NULL,
 connection_id TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY (session_id, repo, remote)
);
CREATE INDEX IF NOT EXISTS idx_git_bindings_connection ON git_bindings(connection_id);
CREATE TABLE IF NOT EXISTS git_defaults (
 user TEXT NOT NULL,
 session_id TEXT NOT NULL DEFAULT '',
 connection_id TEXT NOT NULL,
 PRIMARY KEY(user, session_id)
);
CREATE TABLE IF NOT EXISTS git_operations (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 ts TEXT NOT NULL,
 actor TEXT NOT NULL,
 session_id TEXT NOT NULL DEFAULT '',
 repo TEXT NOT NULL DEFAULT '',
 connection_id TEXT NOT NULL DEFAULT '',
 operation TEXT NOT NULL,
 target TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_git_operations_actor ON git_operations(actor, id);
