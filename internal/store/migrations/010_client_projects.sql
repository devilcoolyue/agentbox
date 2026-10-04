-- Additive desktop metadata. Existing sessions, account bindings and workspace
-- directories retain their semantics; no projects are auto-created or moved.
CREATE TABLE IF NOT EXISTS client_projects (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    name TEXT NOT NULL,
    path TEXT NOT NULL,
    arguments TEXT NOT NULL DEFAULT '[]',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    UNIQUE(session_id, path)
);
CREATE INDEX IF NOT EXISTS idx_client_projects_session ON client_projects(session_id);
CREATE TABLE IF NOT EXISTS client_terminals (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('shell', 'agent')),
    state TEXT NOT NULL DEFAULT 'open' CHECK(state IN ('open', 'closing')),
    arguments TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_client_terminals_session ON client_terminals(session_id);
CREATE INDEX IF NOT EXISTS idx_client_terminals_project ON client_terminals(project_id);
