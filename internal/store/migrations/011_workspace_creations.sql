-- Creation/import receipts survive disconnects and restarts. Keep tombstones
-- after workspace deletion; a replay must never create that workspace again.
CREATE TABLE IF NOT EXISTS workspace_creations (
    user TEXT NOT NULL,
    user_created_at TEXT NOT NULL,
    request_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    request_json TEXT NOT NULL,
    session_json TEXT NOT NULL,
    session_id TEXT NOT NULL UNIQUE,
    git_connection_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('reserved','ready','complete','abandoned')),
    created_at TEXT NOT NULL,
    PRIMARY KEY(user,user_created_at,request_id)
);
CREATE TABLE IF NOT EXISTS workspace_imports (
    user TEXT NOT NULL,
    user_created_at TEXT NOT NULL,
    request_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    kind TEXT NOT NULL,
    directory TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('running','succeeded','failed','uncertain','reviewed')),
    result_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(user,user_created_at,request_id,attempt_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_import_active ON workspace_imports(session_id)
    WHERE state IN ('running','uncertain');
